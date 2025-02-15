// Copyright 2016-2021, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pulumi

import (
	"context"
	"reflect"
	"sort"
	"strings"

	"github.com/pkg/errors"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"

	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"google.golang.org/grpc"
)

type constructFunc func(ctx *Context, typ, name string, inputs map[string]interface{},
	options ResourceOption) (URNInput, Input, error)

// construct adapts the gRPC ConstructRequest/ConstructResponse to/from the Pulumi Go SDK programming model.
func construct(ctx context.Context, req *pulumirpc.ConstructRequest, engineConn *grpc.ClientConn,
	constructF constructFunc) (*pulumirpc.ConstructResponse, error) {

	// Configure the RunInfo.
	runInfo := RunInfo{
		Project:     req.GetProject(),
		Stack:       req.GetStack(),
		Config:      req.GetConfig(),
		Parallel:    int(req.GetParallel()),
		DryRun:      req.GetDryRun(),
		MonitorAddr: req.GetMonitorEndpoint(),
		engineConn:  engineConn,
	}
	pulumiCtx, err := NewContext(ctx, runInfo)
	if err != nil {
		return nil, errors.Wrap(err, "constructing run context")
	}

	// Deserialize the inputs and apply appropriate dependencies.
	inputDependencies := req.GetInputDependencies()
	deserializedInputs, err := plugin.UnmarshalProperties(
		req.GetInputs(),
		plugin.MarshalOptions{KeepSecrets: true, KeepResources: true, KeepUnknowns: req.GetDryRun()},
	)
	if err != nil {
		return nil, errors.Wrap(err, "unmarshaling inputs")
	}
	inputs := make(map[string]interface{}, len(deserializedInputs))
	for key, input := range deserializedInputs {
		k := string(key)
		var deps []Resource
		if inputDeps, ok := inputDependencies[k]; ok {
			deps = make([]Resource, len(inputDeps.GetUrns()))
			for i, depURN := range inputDeps.GetUrns() {
				deps[i] = newDependencyResource(URN(depURN))
			}
		}

		val, secret, err := unmarshalPropertyValue(pulumiCtx, input)
		if err != nil {
			return nil, errors.Wrapf(err, "unmarshaling input %s", k)
		}

		inputs[k] = &constructInput{
			value:  val,
			secret: secret,
			deps:   deps,
		}
	}

	// Rebuild the resource options.
	aliases := make([]Alias, len(req.GetAliases()))
	for i, urn := range req.GetAliases() {
		aliases[i] = Alias{URN: URN(urn)}
	}
	dependencies := make([]Resource, len(req.GetDependencies()))
	for i, urn := range req.GetDependencies() {
		dependencies[i] = newDependencyResource(URN(urn))
	}
	providers := make(map[string]ProviderResource, len(req.GetProviders()))
	for pkg, ref := range req.GetProviders() {
		// Parse the URN and ID out of the provider reference.
		lastSep := strings.LastIndex(ref, "::")
		if lastSep == -1 {
			return nil, errors.Errorf("expected '::' in provider reference %s", ref)
		}
		urn := ref[0:lastSep]
		id := ref[lastSep+2:]
		providers[pkg] = newDependencyProviderResource(URN(urn), ID(id))
	}
	var parent Resource
	if req.GetParent() != "" {
		parent = newDependencyResource(URN(req.GetParent()))
	}
	opts := resourceOption(func(ro *resourceOptions) {
		ro.Aliases = aliases
		ro.DependsOn = dependencies
		ro.Protect = req.GetProtect()
		ro.Providers = providers
		ro.Parent = parent
	})

	urn, state, err := constructF(pulumiCtx, req.GetType(), req.GetName(), inputs, opts)
	if err != nil {
		return nil, err
	}

	// Ensure all outstanding RPCs have completed before proceeding. Also, prevent any new RPCs from happening.
	pulumiCtx.waitForRPCs()
	if pulumiCtx.rpcError != nil {
		return nil, errors.Wrap(pulumiCtx.rpcError, "waiting for RPCs")
	}

	rpcURN, _, _, err := urn.ToURNOutput().awaitURN(ctx)
	if err != nil {
		return nil, err
	}

	// Serialize all state properties, first by awaiting them, and then marshaling them to the requisite gRPC values.
	resolvedProps, propertyDeps, _, err := marshalInputs(state)
	if err != nil {
		return nil, errors.Wrap(err, "marshaling properties")
	}

	// Marshal all properties for the RPC call.
	keepUnknowns := req.GetDryRun()
	rpcProps, err := plugin.MarshalProperties(
		resolvedProps,
		plugin.MarshalOptions{KeepSecrets: true, KeepUnknowns: keepUnknowns, KeepResources: pulumiCtx.keepResources})
	if err != nil {
		return nil, errors.Wrap(err, "marshaling properties")
	}

	// Convert the property dependencies map for RPC and remove duplicates.
	rpcPropertyDeps := make(map[string]*pulumirpc.ConstructResponse_PropertyDependencies)
	for k, deps := range propertyDeps {
		sort.Slice(deps, func(i, j int) bool { return deps[i] < deps[j] })

		urns := make([]string, 0, len(deps))
		for i, d := range deps {
			if i > 0 && urns[i-1] == string(d) {
				continue
			}
			urns = append(urns, string(d))
		}

		rpcPropertyDeps[k] = &pulumirpc.ConstructResponse_PropertyDependencies{
			Urns: urns,
		}
	}

	return &pulumirpc.ConstructResponse{
		Urn:               string(rpcURN),
		State:             rpcProps,
		StateDependencies: rpcPropertyDeps,
	}, nil
}

type constructInput struct {
	value  interface{}
	secret bool
	deps   []Resource
}

// constructInputsMap returns the inputs as a Map.
func constructInputsMap(inputs map[string]interface{}) Map {
	result := make(Map, len(inputs))
	for k, v := range inputs {
		val := v.(*constructInput)
		output := newOutput(anyOutputType, val.deps...)
		output.getState().resolve(val.value, true /*known*/, val.secret, nil)
		result[k] = output
	}
	return result
}

// constructInputsSetArgs sets the inputs on the given args struct.
func constructInputsSetArgs(inputs map[string]interface{}, args interface{}) error {
	if args == nil {
		return errors.New("args must not be nil")
	}
	argsV := reflect.ValueOf(args)
	typ := argsV.Type()
	if typ.Kind() != reflect.Ptr || typ.Elem().Kind() != reflect.Struct {
		return errors.New("args must be a pointer to a struct")
	}
	argsV, typ = argsV.Elem(), typ.Elem()

	for k, v := range inputs {
		val := v.(*constructInput)
		for i := 0; i < typ.NumField(); i++ {
			fieldV := argsV.Field(i)
			if !fieldV.CanSet() {
				continue
			}
			field := typ.Field(i)
			tag, has := field.Tag.Lookup("pulumi")
			if !has || tag != k {
				continue
			}

			if !field.Type.Implements(reflect.TypeOf((*Input)(nil)).Elem()) {
				continue
			}

			outputType := anyOutputType

			toOutputMethodName := "To" + strings.TrimSuffix(field.Type.Name(), "Input") + "Output"
			toOutputMethod, found := field.Type.MethodByName(toOutputMethodName)
			if found {
				mt := toOutputMethod.Type
				if mt.NumIn() != 0 || mt.NumOut() != 1 {
					continue
				}
				outputType = mt.Out(0)
				if !outputType.Implements(reflect.TypeOf((*Output)(nil)).Elem()) {
					continue
				}
			}

			output := newOutput(outputType, val.deps...)
			output.getState().resolve(val.value, true /*known*/, val.secret, nil)
			fieldV.Set(reflect.ValueOf(output))
		}
	}

	return nil
}

// newConstructResult converts a resource into its associated URN and state.
func newConstructResult(resource ComponentResource) (URNInput, Input, error) {
	if resource == nil {
		return nil, nil, errors.New("resource must not be nil")
	}

	resourceV := reflect.ValueOf(resource)
	typ := resourceV.Type()
	if typ.Kind() != reflect.Ptr || typ.Elem().Kind() != reflect.Struct {
		return nil, nil, errors.New("resource must be a pointer to a struct")
	}
	resourceV, typ = resourceV.Elem(), typ.Elem()

	state := make(Map)
	for i := 0; i < typ.NumField(); i++ {
		fieldV := resourceV.Field(i)
		field := typ.Field(i)
		tag, has := field.Tag.Lookup("pulumi")
		if !has {
			continue
		}
		val := fieldV.Interface()
		if v, ok := val.(Input); ok {
			state[tag] = v
		}
	}

	return resource.URN(), state, nil
}
