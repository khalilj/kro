// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package runtime

import (
	"fmt"
	"slices"
	"strings"

	"golang.org/x/exp/maps"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/aws-controllers-k8s/symphony/internal/celutil"
	"github.com/aws-controllers-k8s/symphony/internal/typesystem/resolver"
	"github.com/aws-controllers-k8s/symphony/internal/typesystem/variable"
)

// Compile time proof to ensure that ResourceGroupRuntime implements the
// Runtime interface.
var _ Interface = &ResourceGroupRuntime{}

// NewResourceGroupRuntime creates and initializes a new ResourceGroupRuntime
// instance.
//
// It is also responsible of properly creating the ExpressionEvaluationState
// for each variable in the resources and the instance, and caching them
// for future use. This function will also call Synchronize to evaluate the
// static variables. This helps hide the complexity of the runtime from the
// caller (instance controller in this case).
//
// The output of this function is NOT thread safe.
func NewResourceGroupRuntime(
	instance Resource,
	resources map[string]Resource,
	topologicalOrder []string,
) (*ResourceGroupRuntime, error) {
	r := &ResourceGroupRuntime{
		instance:          instance,
		resources:         resources,
		topologicalOrder:  topologicalOrder,
		resolvedResources: make(map[string]*unstructured.Unstructured),
		runtimeVariables:  make(map[string][]*expressionEvaluationState),
		expressionsCache:  make(map[string]*expressionEvaluationState),
	}
	// make sure to copy the variables and the dependencies, to avoid
	// modifying the original resource.
	for name, resource := range resources {
		// Process the resource variables.
		for _, variable := range resource.GetVariables() {
			for _, expr := range variable.Expressions {
				// If cached use the same pointer.
				if ec, seen := r.expressionsCache[expr]; seen {
					// NOTE(a-hilaly): This strikes me as an early optimization, but
					// it's a good one, i believe... We can always remove it if it's
					// too magical.
					r.runtimeVariables[name] = append(r.runtimeVariables[name], ec)
					continue
				}
				ees := &expressionEvaluationState{
					Expression:   expr,
					Dependencies: variable.Dependencies,
					Kind:         variable.Kind,
				}
				r.runtimeVariables[name] = append(r.runtimeVariables[name], ees)
				r.expressionsCache[expr] = ees
			}
		}
		// Process the readyOnExpressions.
		for _, expr := range resource.GetReadyOnExpressions() {
			ees := &expressionEvaluationState{
				Expression: expr,
				Kind:       variable.ResourceVariableKindDynamic,
			}
			r.expressionsCache[expr] = ees
		}
	}

	// Now we need to collect the instance variables.
	for _, variable := range instance.GetVariables() {
		for _, expr := range variable.Expressions {
			if ec, seen := r.expressionsCache[expr]; seen {
				// It is validated at the Graph level that the resource names
				// can't be `instance`. This is why.
				r.runtimeVariables["instance"] = append(r.runtimeVariables["instance"], ec)
				continue
			}
			ees := &expressionEvaluationState{
				Expression:   expr,
				Dependencies: variable.Dependencies,
				Kind:         variable.Kind,
			}
			r.runtimeVariables["instance"] = append(r.runtimeVariables["instance"], ees)
			r.expressionsCache[expr] = ees
		}
	}

	// Evaluate the static variables, so that the caller only needs to call Synchronize
	// whenever a new resource is added or a variable is updated.
	err := r.evaluateStaticVariables()
	if err != nil {
		return nil, fmt.Errorf("failed to evaluate static variables: %w", err)
	}
	err = r.propagateResourceVariables()
	if err != nil {
		return nil, fmt.Errorf("failed to propagate resource variables: %w", err)
	}

	return r, nil
}

// ResourceGroupRuntime implements the Interface for managing and synchronizing
// resources. Is is the responsibility of the consumer to call Synchronize
// appropriately, and decide whether to follow the TopologicalOrder or a
// BFS/DFS traversal of the resources.
type ResourceGroupRuntime struct {
	// instance represents the main resource instance being managed.
	// This is typically the top-level custom resource that owns or manages
	// other resources in the graph.
	instance Resource

	// resources is a map of all resources in the graph, keyed by their
	// unique identifier. These resources represent the nodes in the
	// dependency graph.
	resources map[string]Resource

	// resolvedResources stores the latest state of resolved resources.
	// When a resource is successfully created or updated in the cluster,
	// its state is stored here. This map helps track which resources have
	// been successfully reconciled with the cluster state.
	resolvedResources map[string]*unstructured.Unstructured

	// runtimeVariables maps resource names to their associated variables.
	// These variables are used in the synchronization process to resolve
	// dependencies and compute derived values for resources.
	runtimeVariables map[string][]*expressionEvaluationState

	// expressionsCache caches evaluated expressions to avoid redundant
	// computations. This optimization helps improve performance by reusing
	// previously calculated results for expressions that haven't changed.
	//
	// NOTE(a-hilaly): It is important to note that the expressionsCache have
	// the same pointers used in the runtimeVariables. Meaning that if a variable
	// is updated here, it will be updated in the runtimeVariables as well, and
	// vice versa.
	expressionsCache map[string]*expressionEvaluationState

	// topologicalOrder holds the dependency order of resources. This order
	// ensures that resources are processed in a way that respects their
	// dependencies, preventing circular dependencies and ensuring efficient
	// synchronization.
	topologicalOrder []string
}

// TopologicalOrder returns the topological order of resources.
func (rt *ResourceGroupRuntime) TopologicalOrder() []string {
	return rt.topologicalOrder
}

// ResourceDescriptor returns the descriptor for a given resource name.
func (rt *ResourceGroupRuntime) ResourceDescriptor(name string) ResourceDescriptor {
	return rt.resources[name]
}

// GetResource returns a resource so that it's either created or updated in
// the cluster, it also returns the runtime state of the resource. Indicating
// whether the resource variables are resolved or not, and whether the resource
// readiness conditions are met or not.
func (rt *ResourceGroupRuntime) GetResource(name string) (*unstructured.Unstructured, ResourceState) {
	// Did the user set the resource?
	r, ok := rt.resolvedResources[name]
	if ok {
		return r, ResourceStateResolved
	}

	// If not, can we process the resource?
	resolved := rt.canProcessResource(name)
	if resolved {
		return rt.resources[name].Unstructured(), ResourceStateResolved
	}

	return nil, ResourceStateWaitingOnDependencies
}

// SetResource updates or sets a resource in the runtime. This is typically
// called after a resource has been created or updated in the cluster.
func (rt *ResourceGroupRuntime) SetResource(name string, resource *unstructured.Unstructured) {
	rt.resolvedResources[name] = resource
}

// GetInstance returns the main instance object managed by this runtime.
func (rt *ResourceGroupRuntime) GetInstance() *unstructured.Unstructured {
	return rt.instance.Unstructured()
}

// SetInstance updates the main instance object.
// This is typically called after the instance has been updated in the cluster.
func (rt *ResourceGroupRuntime) SetInstance(obj *unstructured.Unstructured) {
	ptr := rt.instance.Unstructured()
	ptr.Object = obj.Object
}

// Synchronize tries to resolve as many resources as possible. It returns true
// if the user should call Synchronize again, and false if something is still
// not resolved.
//
// Every time Synchronize is called, it walks through the resources and tries
// to resolve as many as possible. If a resource is resolved, it's added to the
// resolved resources map.
func (rt *ResourceGroupRuntime) Synchronize() (bool, error) {
	// if everything is resolved, we're done.
	// TODO(a-hilaly): Add readiness check here.
	if rt.allExpressionsAreResolved() && len(rt.resolvedResources) == len(rt.resources) {
		return false, nil
	}

	// first synchronize the resources.
	err := rt.evaluateDynamicVariables()
	if err != nil {
		return true, fmt.Errorf("failed to evaluate dynamic variables: %w", err)
	}

	// Now propagate the resource variables.
	err = rt.propagateResourceVariables()
	if err != nil {
		return true, fmt.Errorf("failed to propagate resource variables: %w", err)
	}

	// then synchronize the instance
	err = rt.evaluateInstanceStatuses()
	if err != nil {
		return true, fmt.Errorf("failed to evaluate instance statuses: %w", err)
	}

	return true, nil
}

func (rt *ResourceGroupRuntime) propagateResourceVariables() error {
	for name := range rt.resources {
		if rt.canProcessResource(name) {
			// evaluate the resource variables
			err := rt.evaluateResourceExpressions(name)
			if err != nil {
				return fmt.Errorf("failed to evaluate resource variables for %s: %w", name, err)
			}
		}
	}
	return nil
}

// canProcessResource checks if a resource can be resolved by examining
// if all its dependencies are resolved AND if all its variables are resolved.
func (rt *ResourceGroupRuntime) canProcessResource(resource string) bool {
	// Check if all dependencies are resolved. a.k.a all variables have been
	// evaluated.
	for _, dep := range rt.resources[resource].GetDependencies() {
		if !rt.resourceVariablesResolved(dep) {
			return false
		}
	}

	// Check if the resource variables are resolved.
	kk := rt.resourceVariablesResolved(resource)
	return kk
}

// resourceVariablesResolved determines if all variables for a given resource
// have been resolved.
func (rt *ResourceGroupRuntime) resourceVariablesResolved(resource string) bool {
	for _, variable := range rt.runtimeVariables[resource] {
		if variable.Kind.IsDynamic() && !variable.Resolved {
			return false
		}
	}
	return true
}

// evaluateStaticVariables processes all static variables in the runtime.
// Static variables are those that can be evaluated immediately, typically
// depending only on the initial configuration. This function is usually
// called once during runtime initialization to set up the baseline state
func (rt *ResourceGroupRuntime) evaluateStaticVariables() error {
	env, err := celutil.NewEnvironement(&celutil.EnvironementOptions{
		ResourceNames: []string{"spec"},
	})
	if err != nil {
		return err
	}

	for _, variable := range rt.expressionsCache {
		if variable.Kind.IsStatic() {
			ast, issues := env.Compile(variable.Expression)
			if issues != nil {
				return issues.Err()
			}
			program, err := env.Program(ast)
			if err != nil {
				return err
			}
			val, _, err := program.Eval(map[string]interface{}{
				"spec": rt.instance.Unstructured().Object["spec"],
			})
			if err != nil {
				return err
			}
			value, err := celutil.ConvertCELtoGo(val)
			if err != nil {
				return err
			}

			variable.Resolved = true
			variable.ResolvedValue = value
		}
	}
	return nil
}

type EvalError struct {
	IsIncompleteData bool
	Err              error
}

func (e *EvalError) Error() string {
	if e.IsIncompleteData {
		return fmt.Sprintf("incomplete data: %s", e.Err.Error())
	}
	return e.Err.Error()
}

// evaluateDynamicVariables processes all dynamic variables in the runtime.
// Dynamic variables depend on the state of other resources and are evaluated
// iteratively as resources are resolved. This function is called during each
// synchronization cycle to update the runtime state based on newly resolved
// resources.
func (rt *ResourceGroupRuntime) evaluateDynamicVariables() error {
	// Dynamic variables are those that depend on other resources
	// and are resolved after all the dependencies are resolved.

	resolvedResources := maps.Keys(rt.resolvedResources)
	env, err := celutil.NewEnvironement(&celutil.EnvironementOptions{
		ResourceNames: resolvedResources,
	})
	if err != nil {
		return err
	}

	// let's iterate over any resolved resource and try to resolve
	// the dynamic variables that depend on it.
	// Since we have already cached the expressions, we don't need to
	// loop over all the resources.
	for _, variable := range rt.expressionsCache {
		if variable.Kind.IsDynamic() {
			// Skip the variable if it's already resolved
			if variable.Resolved {
				continue
			}

			// we need to make sure that the dependencies are
			// part of the resolved resources.
			if len(variable.Dependencies) > 0 &&
				!containsAllElements(resolvedResources, variable.Dependencies) {
				continue
			}

			evalContext := make(map[string]interface{})
			for _, dep := range variable.Dependencies {
				evalContext[dep] = rt.resolvedResources[dep].Object
			}
			ast, issues := env.Compile(variable.Expression)
			if issues != nil {
				return issues.Err()
			}
			program, err := env.Program(ast)
			if err != nil {
				return err
			}

			val, _, err := program.Eval(evalContext)
			if err != nil {
				if strings.Contains(err.Error(), "no such key") {
					// TODO(a-hilaly): I'm not sure if this is the best way to handle
					// these. Probably need to reiterate here.
					return &EvalError{
						IsIncompleteData: true,
						Err:              err,
					}
				}
				return &EvalError{
					Err: err,
				}
			}
			value, err := celutil.ConvertCELtoGo(val)
			if err != nil {
				return nil
			}

			variable.Resolved = true
			variable.ResolvedValue = value
		}
	}

	return nil
}

// evaluateInstanceStatuses updates the status of the main instance based on
// the current state of all resources. This function aggregates information
// from all managed resources to provide an overall status of the runtime,
// which is typically reflected in the custom resource's status field.
func (rt *ResourceGroupRuntime) evaluateInstanceStatuses() error {
	rs := resolver.NewResolver(rt.instance.Unstructured().Object, map[string]interface{}{})

	// Two pieces of information are needed here:
	//  1. Instance variables are guaranteed to be standalone expressions.
	//  2. Not all instance variables are guaranteed to be resolved. This is
	//     more like a "best effort" to resolve as many as possible.
	for _, variable := range rt.instance.GetVariables() {
		cached, ok := rt.expressionsCache[variable.Expressions[0]]
		if ok && cached.Resolved {
			err := rs.BlindSetValueAtPath(variable.Path, rt.expressionsCache[variable.Expressions[0]].ResolvedValue)
			if err != nil {
				return fmt.Errorf("failed to set value at path %s: %w", variable.Path, err)
			}
		}
	}
	return nil
}

// evaluateResourceExpressions processes all expressions associated with a
// specific resource.
func (rt *ResourceGroupRuntime) evaluateResourceExpressions(resource string) error {
	exprValues := make(map[string]interface{})
	for _, v := range rt.expressionsCache {
		if v.Resolved {
			exprValues[v.Expression] = v.ResolvedValue
		}
	}

	variables := rt.resources[resource].GetVariables()
	exprFields := make([]variable.FieldDescriptor, len(variables))
	for i, v := range variables {
		exprFields[i] = v.FieldDescriptor
	}

	rs := resolver.NewResolver(rt.resources[resource].Unstructured().Object, exprValues)
	summary := rs.Resolve(exprFields)
	if summary.Errors != nil {
		return fmt.Errorf("failed to resolve resource %s: %v", resource, summary.Errors)
	}
	return nil
}

// allExpressionsAreResolved checks if every expression in the runtimes cache
// has been successfully evaluated
func (rt *ResourceGroupRuntime) allExpressionsAreResolved() bool {
	for _, v := range rt.expressionsCache {
		if !v.Resolved {
			return false
		}
	}
	return true
}

// IsResourceReady checks if a resource is ready based on the readyOnExpressions
// defined in the resource. If no readyOnExpressions are defined, the resource
// is considered ready.
func (rt *ResourceGroupRuntime) IsResourceReady(resourceID string) (bool, error) {
	observed, ok := rt.resolvedResources[resourceID]
	if !ok {
		// Users need to make sure that the resource is resolved a.k.a (SetResource)
		// before calling this function.
		return false, fmt.Errorf("resource %s not found", resourceID)
	}

	expressions := rt.resources[resourceID].GetReadyOnExpressions()
	if len(expressions) == 0 {
		return true, nil
	}

	topLevelFields := rt.resources[resourceID].GetTopLevelFields()
	env, err := celutil.NewEnvironement(&celutil.EnvironementOptions{
		ResourceNames: topLevelFields,
	})

	// we should not expect errors here since we already compiled it
	// in the dryRun
	if err != nil {
		return false, fmt.Errorf("failed creating new Environment: %w", err)
	}
	context := map[string]interface{}{}
	for _, n := range topLevelFields {
		if obj, ok := observed.Object[n]; ok {
			context[n] = obj.(map[string]interface{})
		}
	}
	for _, expression := range expressions {
		// We do want re-evaluate the expression every time, and avoid caching
		// the result. NOTE(a-hilaly): maybe we can cache the result, but for that
		// we also need to define a new Kind for the variables, they are not dynamic
		// nor static. And for sure they need to be expressionEvaluationStateo objects.
		ast, issues := env.Compile(expression)
		if issues != nil && issues.Err() != nil {
			return false, fmt.Errorf("failed compiling expression %s: %w", expression, err)
		}
		program, err := env.Program(ast)
		if err != nil {
			return false, fmt.Errorf("failed programming expression %s: %w", expression, err)
		}

		output, _, err := program.Eval(context)
		if err != nil {
			return false, fmt.Errorf("failed evaluating expression %s: %w", expression, err)
		}
		out, err := celutil.ConvertCELtoGo(output)
		if err != nil {
			return false, fmt.Errorf("failed converting output %v: %w", output, err)
		}
		// keep checking for a false
		if !out.(bool) {
			return false, nil
		}
	}
	return true, err
}

// containsAllElements checks if all elements in the inner slice are present
// in the outer slice.
func containsAllElements[T comparable](outer, inner []T) bool {
	return slices.ContainsFunc(inner, func(v T) bool {
		return slices.Contains(outer, v)
	})
}
