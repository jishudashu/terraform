// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package plugin6

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	plugin "github.com/hashicorp/go-plugin"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
	ctyjson "github.com/zclconf/go-cty/cty/json"
	"github.com/zclconf/go-cty/cty/msgpack"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs/configschema"
	"github.com/hashicorp/terraform/internal/logging"
	"github.com/hashicorp/terraform/internal/plugin6/convert"
	"github.com/hashicorp/terraform/internal/providers"
	proto6 "github.com/hashicorp/terraform/internal/tfplugin6"
)

var logger = logging.HCLogger()

// GRPCProviderPlugin implements plugin.GRPCPlugin for the go-plugin package.
type GRPCProviderPlugin struct {
	plugin.Plugin
	GRPCProvider func() proto6.ProviderServer
}

func (p *GRPCProviderPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return &GRPCProvider{
		client: proto6.NewProviderClient(c),
		ctx:    ctx,
	}, nil
}

func (p *GRPCProviderPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	proto6.RegisterProviderServer(s, p.GRPCProvider())
	return nil
}

// GRPCProvider handles the client, or core side of the plugin rpc connection.
// The GRPCProvider methods are mostly a translation layer between the
// terraform providers types and the grpc proto types, directly converting
// between the two.
type GRPCProvider struct {
	// PluginClient provides a reference to the plugin.Client which controls the plugin process.
	// This allows the GRPCProvider a way to shutdown the plugin process.
	PluginClient *plugin.Client

	// TestServer contains a grpc.Server to close when the GRPCProvider is being
	// used in an end to end test of a provider.
	TestServer *grpc.Server

	// Addr uniquely identifies the type of provider.
	// Normally executed providers will have this set during initialization,
	// but it may not always be available for alternative execute modes.
	Addr addrs.Provider

	// Proto client use to make the grpc service calls.
	client proto6.ProviderClient

	// this context is created by the plugin package, and is canceled when the
	// plugin process ends.
	ctx context.Context

	// schema stores the schema for this provider. This is used to properly
	// serialize the requests for schemas.
	mu     sync.Mutex
	schema providers.GetProviderSchemaResponse
}

func (p *GRPCProvider) GetProviderSchema() providers.GetProviderSchemaResponse {
	p.mu.Lock()
	defer p.mu.Unlock()

	// check the global cache if we can
	if !p.Addr.IsZero() {
		if resp, ok := providers.SchemaCache.Get(p.Addr); ok && resp.ServerCapabilities.GetProviderSchemaOptional {
			logger.Trace("GRPCProvider.v6: returning cached schema", p.Addr.String())
			return resp
		}
	}
	logger.Trace("GRPCProvider.v6: GetProviderSchema")

	// If the local cache is non-zero, we know this instance has called
	// GetProviderSchema at least once and we can return early.
	if p.schema.Provider.Body != nil {
		return p.schema
	}

	var resp providers.GetProviderSchemaResponse

	resp.ResourceTypes = make(map[string]providers.Schema)
	resp.DataSources = make(map[string]providers.Schema)
	resp.EphemeralResourceTypes = make(map[string]providers.Schema)
	resp.ListResourceTypes = make(map[string]providers.Schema)
	resp.StateStores = make(map[string]providers.Schema)
	resp.Actions = make(map[string]providers.ActionSchema)

	// Some providers may generate quite large schemas, and the internal default
	// grpc response size limit is 4MB. 64MB should cover most any use case, and
	// if we get providers nearing that we may want to consider a finer-grained
	// API to fetch individual resource schemas.
	// Note: this option is marked as EXPERIMENTAL in the grpc API. We keep
	// this for compatibility, but recent providers all set the max message
	// size much higher on the server side, which is the supported method for
	// determining payload size.
	const maxRecvSize = 64 << 20
	protoResp, err := p.client.GetProviderSchema(p.ctx, new(proto6.GetProviderSchema_Request), grpc.MaxRecvMsgSizeCallOption{MaxRecvMsgSize: maxRecvSize})
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))

	if resp.Diagnostics.HasErrors() {
		return resp
	}

	if protoResp.Provider == nil {
		resp.Diagnostics = resp.Diagnostics.Append(errors.New("missing provider schema"))
		return resp
	}

	identResp, err := p.client.GetResourceIdentitySchemas(p.ctx, new(proto6.GetResourceIdentitySchemas_Request))
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			// We don't treat this as an error if older providers don't implement this method,
			// so we create an empty map for identity schemas
			identResp = &proto6.GetResourceIdentitySchemas_Response{
				IdentitySchemas: map[string]*proto6.ResourceIdentitySchema{},
			}
		} else {
			resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
			return resp
		}
	}

	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(identResp.Diagnostics))
	if resp.Diagnostics.HasErrors() {
		return resp
	}

	resp.Provider = convert.ProtoToProviderSchema(protoResp.Provider, nil)
	if protoResp.ProviderMeta == nil {
		logger.Debug("No provider meta schema returned")
	} else {
		resp.ProviderMeta = convert.ProtoToProviderSchema(protoResp.ProviderMeta, nil)
	}

	for name, res := range protoResp.ResourceSchemas {
		id := identResp.IdentitySchemas[name] // We're fine if the id is not found
		resp.ResourceTypes[name] = convert.ProtoToProviderSchema(res, id)
	}

	for name, data := range protoResp.DataSourceSchemas {
		resp.DataSources[name] = convert.ProtoToProviderSchema(data, nil)
	}

	for name, ephem := range protoResp.EphemeralResourceSchemas {
		resp.EphemeralResourceTypes[name] = convert.ProtoToProviderSchema(ephem, nil)
	}

	for name, list := range protoResp.ListResourceSchemas {
		ret := convert.ProtoToProviderSchema(list, nil)
		resp.ListResourceTypes[name] = providers.Schema{
			Version: ret.Version,
			Body: &configschema.Block{
				Attributes: map[string]*configschema.Attribute{
					"data": {
						Type:     cty.DynamicPseudoType,
						Computed: true,
					},
				},
				BlockTypes: map[string]*configschema.NestedBlock{
					"config": {
						Block:   *ret.Body,
						Nesting: configschema.NestingSingle,
					},
				},
			},
		}
	}

	for name, store := range protoResp.StateStoreSchemas {
		resp.StateStores[name] = convert.ProtoToProviderSchema(store, nil)
	}

	if decls, err := convert.FunctionDeclsFromProto(protoResp.Functions); err == nil {
		resp.Functions = decls
	} else {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	for name, action := range protoResp.ActionSchemas {
		resp.Actions[name] = convert.ProtoToActionSchema(action)
	}

	if protoResp.ServerCapabilities != nil {
		resp.ServerCapabilities.PlanDestroy = protoResp.ServerCapabilities.PlanDestroy
		resp.ServerCapabilities.GetProviderSchemaOptional = protoResp.ServerCapabilities.GetProviderSchemaOptional
		resp.ServerCapabilities.MoveResourceState = protoResp.ServerCapabilities.MoveResourceState
	}

	// set the global cache if we can
	if !p.Addr.IsZero() {
		providers.SchemaCache.Set(p.Addr, resp)
	}

	// always store this here in the client for providers that are not able to
	// use GetProviderSchemaOptional
	p.schema = resp

	return resp
}

func (p *GRPCProvider) GetResourceIdentitySchemas() providers.GetResourceIdentitySchemasResponse {
	logger.Trace("GRPCProvider.v6: GetResourceIdentitySchemas")

	var resp providers.GetResourceIdentitySchemasResponse

	resp.IdentityTypes = make(map[string]providers.IdentitySchema)

	protoResp, err := p.client.GetResourceIdentitySchemas(p.ctx, new(proto6.GetResourceIdentitySchemas_Request))
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			// We expect no error here if older providers don't implement this method
			return resp
		}

		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))

	if resp.Diagnostics.HasErrors() {
		return resp
	}

	for name, res := range protoResp.IdentitySchemas {
		resp.IdentityTypes[name] = providers.IdentitySchema{
			Version: res.Version,
			Body:    convert.ProtoToIdentitySchema(res.IdentityAttributes),
		}
	}

	return resp
}

func (p *GRPCProvider) ValidateProviderConfig(r providers.ValidateProviderConfigRequest) (resp providers.ValidateProviderConfigResponse) {
	logger.Trace("GRPCProvider.v6: ValidateProviderConfig")

	schema := p.GetProviderSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}

	ty := schema.Provider.Body.ImpliedType()

	mp, err := msgpack.Marshal(r.Config, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &proto6.ValidateProviderConfig_Request{
		Config: &proto6.DynamicValue{Msgpack: mp},
	}

	protoResp, err := p.client.ValidateProviderConfig(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))
	return resp
}

func (p *GRPCProvider) ValidateResourceConfig(r providers.ValidateResourceConfigRequest) (resp providers.ValidateResourceConfigResponse) {
	logger.Trace("GRPCProvider.v6: ValidateResourceConfig")

	schema := p.GetProviderSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}

	resourceSchema, ok := schema.ResourceTypes[r.TypeName]
	if !ok {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown resource type %q", r.TypeName))
		return resp
	}

	mp, err := msgpack.Marshal(r.Config, resourceSchema.Body.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &proto6.ValidateResourceConfig_Request{
		TypeName:           r.TypeName,
		Config:             &proto6.DynamicValue{Msgpack: mp},
		ClientCapabilities: clientCapabilitiesToProto(r.ClientCapabilities),
	}

	protoResp, err := p.client.ValidateResourceConfig(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))
	return resp
}

func (p *GRPCProvider) ValidateDataResourceConfig(r providers.ValidateDataResourceConfigRequest) (resp providers.ValidateDataResourceConfigResponse) {
	logger.Trace("GRPCProvider.v6: ValidateDataResourceConfig")

	schema := p.GetProviderSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}

	dataSchema, ok := schema.DataSources[r.TypeName]
	if !ok {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown data source %q", r.TypeName))
		return resp
	}

	mp, err := msgpack.Marshal(r.Config, dataSchema.Body.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &proto6.ValidateDataResourceConfig_Request{
		TypeName: r.TypeName,
		Config:   &proto6.DynamicValue{Msgpack: mp},
	}

	protoResp, err := p.client.ValidateDataResourceConfig(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}
	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))
	return resp
}

func (p *GRPCProvider) ValidateListResourceConfig(r providers.ValidateListResourceConfigRequest) (resp providers.ValidateListResourceConfigResponse) {
	logger.Trace("GRPCProvider.v6: ValidateListResourceConfig")

	schema := p.GetProviderSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}

	listResourceSchema, ok := schema.ListResourceTypes[r.TypeName]
	if !ok {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown list resource type %q", r.TypeName))
		return resp
	}
	configSchema := listResourceSchema.Body.BlockTypes["config"]
	config := cty.NullVal(configSchema.ImpliedType())
	if r.Config.Type().HasAttribute("config") {
		config = r.Config.GetAttr("config")
	}
	mp, err := msgpack.Marshal(config, configSchema.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &proto6.ValidateListResourceConfig_Request{
		TypeName: r.TypeName,
		Config:   &proto6.DynamicValue{Msgpack: mp},
	}

	protoResp, err := p.client.ValidateListResourceConfig(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))
	return resp
}

func (p *GRPCProvider) UpgradeResourceState(r providers.UpgradeResourceStateRequest) (resp providers.UpgradeResourceStateResponse) {
	logger.Trace("GRPCProvider.v6: UpgradeResourceState")

	schema := p.GetProviderSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}

	resSchema, ok := schema.ResourceTypes[r.TypeName]
	if !ok {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown resource type %q", r.TypeName))
		return resp
	}

	protoReq := &proto6.UpgradeResourceState_Request{
		TypeName: r.TypeName,
		Version:  int64(r.Version),
		RawState: &proto6.RawState{
			Json:    r.RawStateJSON,
			Flatmap: r.RawStateFlatmap,
		},
	}

	protoResp, err := p.client.UpgradeResourceState(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}
	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))

	ty := resSchema.Body.ImpliedType()
	resp.UpgradedState = cty.NullVal(ty)
	if protoResp.UpgradedState == nil {
		return resp
	}

	state, err := decodeDynamicValue(protoResp.UpgradedState, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	resp.UpgradedState = state

	return resp
}

func (p *GRPCProvider) UpgradeResourceIdentity(r providers.UpgradeResourceIdentityRequest) (resp providers.UpgradeResourceIdentityResponse) {
	logger.Trace("GRPCProvider.v6: UpgradeResourceIdentity")

	schema := p.GetProviderSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}

	resSchema, ok := schema.ResourceTypes[r.TypeName]
	if !ok {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown resource identity type %q", r.TypeName))
		return resp
	}

	protoReq := &proto6.UpgradeResourceIdentity_Request{
		TypeName: r.TypeName,
		Version:  int64(r.Version),
		RawIdentity: &proto6.RawState{
			Json: r.RawIdentityJSON,
		},
	}

	protoResp, err := p.client.UpgradeResourceIdentity(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}
	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))

	ty := resSchema.Identity.ImpliedType()
	resp.UpgradedIdentity = cty.NullVal(ty)
	if protoResp.UpgradedIdentity == nil {
		return resp
	}

	identity, err := decodeDynamicValue(protoResp.UpgradedIdentity.IdentityData, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	resp.UpgradedIdentity = identity

	return resp
}

func (p *GRPCProvider) ConfigureProvider(r providers.ConfigureProviderRequest) (resp providers.ConfigureProviderResponse) {
	logger.Trace("GRPCProvider.v6: ConfigureProvider")

	schema := p.GetProviderSchema()

	var mp []byte

	// we don't have anything to marshal if there's no config
	mp, err := msgpack.Marshal(r.Config, schema.Provider.Body.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &proto6.ConfigureProvider_Request{
		TerraformVersion: r.TerraformVersion,
		Config: &proto6.DynamicValue{
			Msgpack: mp,
		},
		ClientCapabilities: clientCapabilitiesToProto(r.ClientCapabilities),
	}

	protoResp, err := p.client.ConfigureProvider(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}
	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))
	return resp
}

func (p *GRPCProvider) Stop() error {
	logger.Trace("GRPCProvider.v6: Stop")

	resp, err := p.client.StopProvider(p.ctx, new(proto6.StopProvider_Request))
	if err != nil {
		return err
	}

	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	return nil
}

func (p *GRPCProvider) ReadResource(r providers.ReadResourceRequest) (resp providers.ReadResourceResponse) {
	logger.Trace("GRPCProvider.v6: ReadResource")

	schema := p.GetProviderSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}

	resSchema, ok := schema.ResourceTypes[r.TypeName]
	if !ok {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown resource type %s", r.TypeName))
		return resp
	}

	metaSchema := schema.ProviderMeta

	mp, err := msgpack.Marshal(r.PriorState, resSchema.Body.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &proto6.ReadResource_Request{
		TypeName:           r.TypeName,
		CurrentState:       &proto6.DynamicValue{Msgpack: mp},
		Private:            r.Private,
		ClientCapabilities: clientCapabilitiesToProto(r.ClientCapabilities),
	}

	if metaSchema.Body != nil {
		metaMP, err := msgpack.Marshal(r.ProviderMeta, metaSchema.Body.ImpliedType())
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}
		protoReq.ProviderMeta = &proto6.DynamicValue{Msgpack: metaMP}
	}

	if !r.CurrentIdentity.IsNull() {
		if resSchema.Identity == nil {
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("identity type not found for resource type %s", r.TypeName))
			return resp
		}
		currentIdentityMP, err := msgpack.Marshal(r.CurrentIdentity, resSchema.Identity.ImpliedType())
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}
		protoReq.CurrentIdentity = &proto6.ResourceIdentityData{
			IdentityData: &proto6.DynamicValue{Msgpack: currentIdentityMP},
		}
	}

	protoResp, err := p.client.ReadResource(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}
	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))

	state, err := decodeDynamicValue(protoResp.NewState, resSchema.Body.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	resp.NewState = state
	resp.Private = protoResp.Private
	resp.Deferred = convert.ProtoToDeferred(protoResp.Deferred)

	if protoResp.NewIdentity != nil && protoResp.NewIdentity.IdentityData != nil {

		if resSchema.Identity == nil {
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown identity type %q", r.TypeName))
		}

		resp.Identity, err = decodeDynamicValue(protoResp.NewIdentity.IdentityData, resSchema.Identity.ImpliedType())
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
		}
	}

	return resp
}

func (p *GRPCProvider) PlanResourceChange(r providers.PlanResourceChangeRequest) (resp providers.PlanResourceChangeResponse) {
	logger.Trace("GRPCProvider.v6: PlanResourceChange")

	schema := p.GetProviderSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}

	resSchema, ok := schema.ResourceTypes[r.TypeName]
	if !ok {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown resource type %q", r.TypeName))
		return resp
	}

	metaSchema := schema.ProviderMeta
	capabilities := schema.ServerCapabilities

	// If the provider doesn't support planning a destroy operation, we can
	// return immediately.
	if r.ProposedNewState.IsNull() && !capabilities.PlanDestroy {
		resp.PlannedState = r.ProposedNewState
		resp.PlannedPrivate = r.PriorPrivate
		return resp
	}

	priorMP, err := msgpack.Marshal(r.PriorState, resSchema.Body.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	configMP, err := msgpack.Marshal(r.Config, resSchema.Body.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	propMP, err := msgpack.Marshal(r.ProposedNewState, resSchema.Body.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &proto6.PlanResourceChange_Request{
		TypeName:           r.TypeName,
		PriorState:         &proto6.DynamicValue{Msgpack: priorMP},
		Config:             &proto6.DynamicValue{Msgpack: configMP},
		ProposedNewState:   &proto6.DynamicValue{Msgpack: propMP},
		PriorPrivate:       r.PriorPrivate,
		ClientCapabilities: clientCapabilitiesToProto(r.ClientCapabilities),
	}

	if metaSchema.Body != nil {
		metaTy := metaSchema.Body.ImpliedType()
		metaVal := r.ProviderMeta
		if metaVal == cty.NilVal {
			metaVal = cty.NullVal(metaTy)
		}
		metaMP, err := msgpack.Marshal(metaVal, metaTy)
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}
		protoReq.ProviderMeta = &proto6.DynamicValue{Msgpack: metaMP}
	}

	if !r.PriorIdentity.IsNull() {
		if resSchema.Identity == nil {
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("identity type not found for resource type %q", r.TypeName))
			return resp
		}
		priorIdentityMP, err := msgpack.Marshal(r.PriorIdentity, resSchema.Identity.ImpliedType())
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}
		protoReq.PriorIdentity = &proto6.ResourceIdentityData{
			IdentityData: &proto6.DynamicValue{Msgpack: priorIdentityMP},
		}
	}

	protoResp, err := p.client.PlanResourceChange(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}
	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))

	state, err := decodeDynamicValue(protoResp.PlannedState, resSchema.Body.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	resp.PlannedState = state

	for _, p := range protoResp.RequiresReplace {
		resp.RequiresReplace = append(resp.RequiresReplace, convert.AttributePathToPath(p))
	}

	resp.PlannedPrivate = protoResp.PlannedPrivate

	resp.LegacyTypeSystem = protoResp.LegacyTypeSystem

	resp.Deferred = convert.ProtoToDeferred(protoResp.Deferred)

	if protoResp.PlannedIdentity != nil && protoResp.PlannedIdentity.IdentityData != nil {
		if resSchema.Identity == nil {
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown identity type %s", r.TypeName))
			return resp
		}

		resp.PlannedIdentity, err = decodeDynamicValue(protoResp.PlannedIdentity.IdentityData, resSchema.Identity.ImpliedType())
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}
	}

	return resp
}

func (p *GRPCProvider) ApplyResourceChange(r providers.ApplyResourceChangeRequest) (resp providers.ApplyResourceChangeResponse) {
	logger.Trace("GRPCProvider.v6: ApplyResourceChange")

	schema := p.GetProviderSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}

	resSchema, ok := schema.ResourceTypes[r.TypeName]
	if !ok {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown resource type %q", r.TypeName))
		return resp
	}

	metaSchema := schema.ProviderMeta

	priorMP, err := msgpack.Marshal(r.PriorState, resSchema.Body.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	plannedMP, err := msgpack.Marshal(r.PlannedState, resSchema.Body.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	configMP, err := msgpack.Marshal(r.Config, resSchema.Body.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &proto6.ApplyResourceChange_Request{
		TypeName:       r.TypeName,
		PriorState:     &proto6.DynamicValue{Msgpack: priorMP},
		PlannedState:   &proto6.DynamicValue{Msgpack: plannedMP},
		Config:         &proto6.DynamicValue{Msgpack: configMP},
		PlannedPrivate: r.PlannedPrivate,
	}

	if metaSchema.Body != nil {
		metaTy := metaSchema.Body.ImpliedType()
		metaVal := r.ProviderMeta
		if metaVal == cty.NilVal {
			metaVal = cty.NullVal(metaTy)
		}
		metaMP, err := msgpack.Marshal(metaVal, metaTy)
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}
		protoReq.ProviderMeta = &proto6.DynamicValue{Msgpack: metaMP}
	}

	if !r.PlannedIdentity.IsNull() {
		if resSchema.Identity == nil {
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("identity type not found for resource type %s", r.TypeName))
			return resp
		}
		identityMP, err := msgpack.Marshal(r.PlannedIdentity, resSchema.Identity.ImpliedType())
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}
		protoReq.PlannedIdentity = &proto6.ResourceIdentityData{
			IdentityData: &proto6.DynamicValue{Msgpack: identityMP},
		}
	}

	protoResp, err := p.client.ApplyResourceChange(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}
	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))

	resp.Private = protoResp.Private

	state, err := decodeDynamicValue(protoResp.NewState, resSchema.Body.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	resp.NewState = state

	resp.LegacyTypeSystem = protoResp.LegacyTypeSystem

	if protoResp.NewIdentity != nil && protoResp.NewIdentity.IdentityData != nil {
		if resSchema.Identity == nil {
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("identity type not found for resource type %s", r.TypeName))
			return resp
		}
		newIdentity, err := decodeDynamicValue(protoResp.NewIdentity.IdentityData, resSchema.Identity.ImpliedType())
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}
		resp.NewIdentity = newIdentity
	}

	return resp
}

func (p *GRPCProvider) ImportResourceState(r providers.ImportResourceStateRequest) (resp providers.ImportResourceStateResponse) {
	logger.Trace("GRPCProvider.v6: ImportResourceState")

	schema := p.GetProviderSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}

	protoReq := &proto6.ImportResourceState_Request{
		TypeName:           r.TypeName,
		Id:                 r.ID,
		ClientCapabilities: clientCapabilitiesToProto(r.ClientCapabilities),
	}

	if !r.Identity.IsNull() {
		resSchema := schema.ResourceTypes[r.TypeName]
		if resSchema.Identity == nil {
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown identity type %q", r.TypeName))
			return resp
		}

		mp, err := msgpack.Marshal(r.Identity, resSchema.Identity.ImpliedType())
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}

		protoReq.Identity = &proto6.ResourceIdentityData{
			IdentityData: &proto6.DynamicValue{
				Msgpack: mp,
			},
		}
	}

	protoResp, err := p.client.ImportResourceState(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}
	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))
	resp.Deferred = convert.ProtoToDeferred(protoResp.Deferred)

	for _, imported := range protoResp.ImportedResources {
		resource := providers.ImportedResource{
			TypeName: imported.TypeName,
			Private:  imported.Private,
		}

		resSchema, ok := schema.ResourceTypes[r.TypeName]
		if !ok {
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown resource type %q", r.TypeName))
			continue
		}

		state, err := decodeDynamicValue(imported.State, resSchema.Body.ImpliedType())
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}
		resource.State = state

		if imported.Identity != nil && imported.Identity.IdentityData != nil {
			importedIdentitySchema, ok := schema.ResourceTypes[imported.TypeName]
			if !ok {
				resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown resource type %q", imported.TypeName))
				continue
			}
			importedIdentity, err := decodeDynamicValue(imported.Identity.IdentityData, importedIdentitySchema.Identity.ImpliedType())
			if err != nil {
				resp.Diagnostics = resp.Diagnostics.Append(err)
				return resp
			}
			resource.Identity = importedIdentity
		}

		resp.ImportedResources = append(resp.ImportedResources, resource)
	}

	return resp
}

func (p *GRPCProvider) MoveResourceState(r providers.MoveResourceStateRequest) (resp providers.MoveResourceStateResponse) {
	logger.Trace("GRPCProvider: MoveResourceState")

	var sourceIdentity *proto6.RawState
	if len(r.SourceIdentity) > 0 {
		sourceIdentity = &proto6.RawState{
			Json: r.SourceIdentity,
		}
	}

	protoReq := &proto6.MoveResourceState_Request{
		SourceProviderAddress: r.SourceProviderAddress,
		SourceTypeName:        r.SourceTypeName,
		SourceSchemaVersion:   r.SourceSchemaVersion,
		SourceState: &proto6.RawState{
			Json: r.SourceStateJSON,
		},
		SourcePrivate:  r.SourcePrivate,
		SourceIdentity: sourceIdentity,
		TargetTypeName: r.TargetTypeName,
	}

	protoResp, err := p.client.MoveResourceState(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}
	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))
	if resp.Diagnostics.HasErrors() {
		return resp
	}

	schema := p.GetProviderSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}

	targetType, ok := schema.ResourceTypes[r.TargetTypeName]
	if !ok {
		// We should have validated this earlier in the process, but we'll
		// still return an error instead of crashing in case something went
		// wrong.
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown resource type %q; this is a bug in Terraform - please report it", r.TargetTypeName))
		return resp
	}
	resp.TargetState, err = decodeDynamicValue(protoResp.TargetState, targetType.Body.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	resp.TargetPrivate = protoResp.TargetPrivate

	if protoResp.TargetIdentity != nil && protoResp.TargetIdentity.IdentityData != nil {
		targetResSchema := schema.ResourceTypes[r.TargetTypeName]

		if targetResSchema.Identity == nil {
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown identity type %s", r.TargetTypeName))
			return resp
		}
		resp.TargetIdentity, err = decodeDynamicValue(protoResp.TargetIdentity.IdentityData, targetResSchema.Identity.ImpliedType())
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}
	}

	return resp
}

func (p *GRPCProvider) ReadDataSource(r providers.ReadDataSourceRequest) (resp providers.ReadDataSourceResponse) {
	logger.Trace("GRPCProvider.v6: ReadDataSource")

	schema := p.GetProviderSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}

	dataSchema, ok := schema.DataSources[r.TypeName]
	if !ok {
		schema.Diagnostics = schema.Diagnostics.Append(fmt.Errorf("unknown data source %q", r.TypeName))
	}

	metaSchema := schema.ProviderMeta

	config, err := msgpack.Marshal(r.Config, dataSchema.Body.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &proto6.ReadDataSource_Request{
		TypeName: r.TypeName,
		Config: &proto6.DynamicValue{
			Msgpack: config,
		},
		ClientCapabilities: clientCapabilitiesToProto(r.ClientCapabilities),
	}

	if metaSchema.Body != nil {
		metaMP, err := msgpack.Marshal(r.ProviderMeta, metaSchema.Body.ImpliedType())
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}
		protoReq.ProviderMeta = &proto6.DynamicValue{Msgpack: metaMP}
	}

	protoResp, err := p.client.ReadDataSource(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}
	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))

	state, err := decodeDynamicValue(protoResp.State, dataSchema.Body.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	resp.State = state
	resp.Deferred = convert.ProtoToDeferred(protoResp.Deferred)

	return resp
}

func (p *GRPCProvider) ValidateEphemeralResourceConfig(r providers.ValidateEphemeralResourceConfigRequest) (resp providers.ValidateEphemeralResourceConfigResponse) {
	logger.Trace("GRPCProvider.v6: ValidateEphemeralResourceConfig")

	schema := p.GetProviderSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}

	ephemSchema, ok := schema.EphemeralResourceTypes[r.TypeName]
	if !ok {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown ephemeral resource %q", r.TypeName))
		return resp
	}

	mp, err := msgpack.Marshal(r.Config, ephemSchema.Body.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &proto6.ValidateEphemeralResourceConfig_Request{
		TypeName: r.TypeName,
		Config:   &proto6.DynamicValue{Msgpack: mp},
	}

	protoResp, err := p.client.ValidateEphemeralResourceConfig(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}
	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))
	return resp
}

func (p *GRPCProvider) OpenEphemeralResource(r providers.OpenEphemeralResourceRequest) (resp providers.OpenEphemeralResourceResponse) {
	logger.Trace("GRPCProvide.v6: OpenEphemeralResource")

	schema := p.GetProviderSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}

	ephemSchema, ok := schema.EphemeralResourceTypes[r.TypeName]
	if !ok {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown ephemeral resource %q", r.TypeName))
		return resp
	}

	config, err := msgpack.Marshal(r.Config, ephemSchema.Body.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &proto6.OpenEphemeralResource_Request{
		TypeName: r.TypeName,
		Config: &proto6.DynamicValue{
			Msgpack: config,
		},
		ClientCapabilities: clientCapabilitiesToProto(r.ClientCapabilities),
	}

	protoResp, err := p.client.OpenEphemeralResource(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}
	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))

	state, err := decodeDynamicValue(protoResp.Result, ephemSchema.Body.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	if protoResp.RenewAt != nil {
		resp.RenewAt = protoResp.RenewAt.AsTime()
	}

	resp.Result = state
	resp.Private = protoResp.Private
	resp.Deferred = convert.ProtoToDeferred(protoResp.Deferred)

	return resp
}

func (p *GRPCProvider) RenewEphemeralResource(r providers.RenewEphemeralResourceRequest) (resp providers.RenewEphemeralResourceResponse) {
	logger.Trace("GRPCProvider.v6: RenewEphemeralResource")

	protoReq := &proto6.RenewEphemeralResource_Request{
		TypeName: r.TypeName,
		Private:  r.Private,
	}

	protoResp, err := p.client.RenewEphemeralResource(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}
	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))

	if protoResp.RenewAt != nil {
		resp.RenewAt = protoResp.RenewAt.AsTime()
	}

	resp.Private = protoResp.Private

	return resp
}

func (p *GRPCProvider) CloseEphemeralResource(r providers.CloseEphemeralResourceRequest) (resp providers.CloseEphemeralResourceResponse) {
	logger.Trace("GRPCProvider.v6: CloseEphemeralResource")

	protoReq := &proto6.CloseEphemeralResource_Request{
		TypeName: r.TypeName,
		Private:  r.Private,
	}

	protoResp, err := p.client.CloseEphemeralResource(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}
	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))

	return resp
}

func (p *GRPCProvider) CallFunction(r providers.CallFunctionRequest) (resp providers.CallFunctionResponse) {
	logger.Trace("GRPCProvider.v6", "CallFunction", r.FunctionName)

	schema := p.GetProviderSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Err = schema.Diagnostics.Err()
		return resp
	}

	funcDecl, ok := schema.Functions[r.FunctionName]
	// We check for various problems with the request below in the interests
	// of robustness, just to avoid crashing while trying to encode/decode, but
	// if we reach any of these errors then that suggests a bug in the caller,
	// because we should catch function calls that don't match the schema at an
	// earlier point than this.
	if !ok {
		// Should only get here if the caller has a bug, because we should
		// have detected earlier any attempt to call a function that the
		// provider didn't declare.
		resp.Err = fmt.Errorf("provider has no function named %q", r.FunctionName)
		return resp
	}
	if len(r.Arguments) < len(funcDecl.Parameters) {
		resp.Err = fmt.Errorf("not enough arguments for function %q", r.FunctionName)
		return resp
	}
	if funcDecl.VariadicParameter == nil && len(r.Arguments) > len(funcDecl.Parameters) {
		resp.Err = fmt.Errorf("too many arguments for function %q", r.FunctionName)
		return resp
	}
	args := make([]*proto6.DynamicValue, len(r.Arguments))
	for i, argVal := range r.Arguments {
		var paramDecl providers.FunctionParam
		if i < len(funcDecl.Parameters) {
			paramDecl = funcDecl.Parameters[i]
		} else {
			paramDecl = *funcDecl.VariadicParameter
		}

		argValRaw, err := msgpack.Marshal(argVal, paramDecl.Type)
		if err != nil {
			resp.Err = err
			return resp
		}
		args[i] = &proto6.DynamicValue{
			Msgpack: argValRaw,
		}
	}

	protoResp, err := p.client.CallFunction(p.ctx, &proto6.CallFunction_Request{
		Name:      r.FunctionName,
		Arguments: args,
	})
	if err != nil {
		// functions can only support simple errors, but use our grpcError
		// diagnostic function to format common problems is a more
		// user-friendly manner.
		resp.Err = grpcErr(err).Err()
		return resp
	}

	if protoResp.Error != nil {
		resp.Err = errors.New(protoResp.Error.Text)

		// If this is a problem with a specific argument, we can wrap the error
		// in a function.ArgError
		if protoResp.Error.FunctionArgument != nil {
			resp.Err = function.NewArgError(int(*protoResp.Error.FunctionArgument), resp.Err)
		}

		return resp
	}

	resultVal, err := decodeDynamicValue(protoResp.Result, funcDecl.ReturnType)
	if err != nil {
		resp.Err = err
		return resp
	}

	resp.Result = resultVal
	return resp
}

func (p *GRPCProvider) ListResource(r providers.ListResourceRequest) providers.ListResourceResponse {
	logger.Trace("GRPCProvider.v6: ListResource")
	var resp providers.ListResourceResponse

	schema := p.GetProviderSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}

	listResourceSchema, ok := schema.ListResourceTypes[r.TypeName]
	if !ok {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown list resource type %q", r.TypeName))
		return resp
	}

	resourceSchema, ok := schema.ResourceTypes[r.TypeName]
	if !ok || resourceSchema.Identity == nil {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("identity schema not found for resource type %s", r.TypeName))
		return resp
	}

	configSchema := listResourceSchema.Body.BlockTypes["config"]
	config := cty.NullVal(configSchema.ImpliedType())
	if r.Config.Type().HasAttribute("config") {
		config = r.Config.GetAttr("config")
	}
	mp, err := msgpack.Marshal(config, configSchema.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &proto6.ListResource_Request{
		TypeName:              r.TypeName,
		Config:                &proto6.DynamicValue{Msgpack: mp},
		IncludeResourceObject: r.IncludeResourceObject,
		Limit:                 r.Limit,
	}

	// Start the streaming RPC with a context. The context will be cancelled
	// when this function returns, which will stop the stream if it is still
	// running.
	ctx, cancel := context.WithCancel(p.ctx)
	defer cancel()
	client, err := p.client.ListResource(ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	resp.Result = cty.DynamicVal
	results := make([]cty.Value, 0)
	// Process the stream
	for {
		if int64(len(results)) >= r.Limit {
			// If we have reached the limit, we stop receiving events
			break
		}

		event, err := client.Recv()
		if err == io.EOF {
			// End of stream, we're done
			break
		}

		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			break
		}

		obj := map[string]cty.Value{
			"display_name": cty.StringVal(event.DisplayName),
			"state":        cty.NullVal(resourceSchema.Body.ImpliedType()),
			"identity":     cty.NullVal(resourceSchema.Identity.ImpliedType()),
		}
		resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(event.Diagnostic))

		// Handle identity data - it must be present
		if event.Identity == nil || event.Identity.IdentityData == nil {
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("missing identity data in ListResource event for %s", r.TypeName))
		} else {
			identityVal, err := decodeDynamicValue(event.Identity.IdentityData, resourceSchema.Identity.ImpliedType())
			if err != nil {
				resp.Diagnostics = resp.Diagnostics.Append(err)
			} else {
				obj["identity"] = identityVal
			}
		}

		// Handle resource object if present and requested
		if event.ResourceObject != nil && r.IncludeResourceObject {
			// Use the ResourceTypes schema for the resource object
			resourceObj, err := decodeDynamicValue(event.ResourceObject, resourceSchema.Body.ImpliedType())
			if err != nil {
				resp.Diagnostics = resp.Diagnostics.Append(err)
			} else {
				obj["state"] = resourceObj
			}
		}

		if resp.Diagnostics.HasErrors() {
			break
		}

		results = append(results, cty.ObjectVal(obj))

	}

	// The provider result of a list resource is always a list, but
	// we will wrap that list in an object with a single attribute "data",
	// so that we can differentiate between a list resource instance (list.aws_instance.test[index])
	// and the elements of the result of a list resource instance (list.aws_instance.test.data[index])
	resp.Result = cty.ObjectVal(map[string]cty.Value{
		"data":   cty.TupleVal(results),
		"config": config,
	})
	return resp
}

func (p *GRPCProvider) ValidateStateStoreConfig(r providers.ValidateStateStoreConfigRequest) providers.ValidateStateStoreConfigResponse {
	panic("not implemented")
}

func (p *GRPCProvider) ConfigureStateStore(r providers.ConfigureStateStoreRequest) providers.ConfigureStateStoreResponse {
	panic("not implemented")
}

func (p *GRPCProvider) GetStates(r providers.GetStatesRequest) providers.GetStatesResponse {
	panic("not implemented")
}

func (p *GRPCProvider) DeleteState(r providers.DeleteStateRequest) providers.DeleteStateResponse {
	panic("not implemented")
}

// closing the grpc connection is final, and terraform will call it at the end of every phase.
func (p *GRPCProvider) Close() error {
	logger.Trace("GRPCProvider.v6: Close")

	// Make sure to stop the server if we're not running within go-plugin.
	if p.TestServer != nil {
		p.TestServer.Stop()
	}

	// Check this since it's not automatically inserted during plugin creation.
	// It's currently only inserted by the command package, because that is
	// where the factory is built and is the only point with access to the
	// plugin.Client.
	if p.PluginClient == nil {
		logger.Debug("provider has no plugin.Client")
		return nil
	}

	p.PluginClient.Kill()
	return nil
}

func (p *GRPCProvider) PlanAction(r providers.PlanActionRequest) (resp providers.PlanActionResponse) {
	logger.Trace("GRPCProvider: PlanAction")

	schema := p.GetProviderSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}

	actionSchema, ok := schema.Actions[r.ActionType]
	if !ok {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown action %q", r.ActionType))
		return resp
	}

	configMP, err := msgpack.Marshal(r.ProposedActionData, actionSchema.ConfigSchema.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	linkedResources, err := linkedResourcePlanDataToProto(schema, actionSchema.LinkedResources(), r.LinkedResources)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &proto6.PlanAction_Request{
		ActionType:         r.ActionType,
		Config:             &proto6.DynamicValue{Msgpack: configMP},
		LinkedResources:    linkedResources,
		ClientCapabilities: clientCapabilitiesToProto(r.ClientCapabilities),
	}

	protoResp, err := p.client.PlanAction(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}
	if resp.Diagnostics.HasErrors() {
		return resp
	}

	resp.LinkedResources, err = protoToLinkedResourcePlans(schema, actionSchema.LinkedResources(), protoResp.LinkedResources)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}

	return resp
}

func (p *GRPCProvider) InvokeAction(r providers.InvokeActionRequest) (resp providers.InvokeActionResponse) {
	logger.Trace("GRPCProvider: InvokeAction")

	schema := p.GetProviderSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}

	actionSchema, ok := schema.Actions[r.ActionType]
	if !ok {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown action %q", r.ActionType))
		return resp
	}

	linkedResources, err := linkedResourceInvokeDataToProto(schema, actionSchema.LinkedResources(), r.LinkedResources)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	configMP, err := msgpack.Marshal(r.PlannedActionData, actionSchema.ConfigSchema.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &proto6.InvokeAction_Request{
		ActionType:      r.ActionType,
		Config:          &proto6.DynamicValue{Msgpack: configMP},
		LinkedResources: linkedResources,
	}

	protoClient, err := p.client.InvokeAction(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}

	resp.Events = func(yield func(providers.InvokeActionEvent) bool) {
		logger.Trace("GRPCProvider: InvokeAction: streaming events")

		for {
			event, err := protoClient.Recv()
			if err == io.EOF {
				logger.Trace("GRPCProvider: InvokeAction: end of stream")
				break
			}
			if err != nil {
				// We handle this by returning a finished response with the error
				// If the client errors we won't be receiving any more events.
				yield(providers.InvokeActionEvent_Completed{
					Diagnostics: grpcErr(err),
				})
				break
			}

			switch ev := event.Type.(type) {
			case *proto6.InvokeAction_Event_Progress_:
				yield(providers.InvokeActionEvent_Progress{
					Message: ev.Progress.Message,
				})

			case *proto6.InvokeAction_Event_Completed_:
				diags := convert.ProtoToDiagnostics(ev.Completed.Diagnostics)
				linkedResources, err := protoToLinkedResourceResults(schema, actionSchema.LinkedResources(), ev.Completed.LinkedResources)
				if err != nil {
					diags = diags.Append(grpcErr(err))
				}
				yield(providers.InvokeActionEvent_Completed{
					LinkedResources: linkedResources,
					Diagnostics:     diags,
				})

			default:
				panic(fmt.Sprintf("unexpected event type %T in InvokeAction response", event.Type))
			}
		}
	}

	return resp
}

func (p *GRPCProvider) ValidateActionConfig(r providers.ValidateActionConfigRequest) (resp providers.ValidateActionConfigResponse) {
	logger.Trace("GRPCProvider.v6: ValidateActionConfig")

	schema := p.GetProviderSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}

	action, ok := schema.Actions[r.TypeName]
	if !ok {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown data source %q", r.TypeName))
		return resp
	}

	mp, err := msgpack.Marshal(r.Config, action.ConfigSchema.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &proto6.ValidateActionConfig_Request{
		TypeName: r.TypeName,
		Config:   &proto6.DynamicValue{Msgpack: mp},
	}

	lrs := make([]*proto6.LinkedResourceConfig, 0, len(r.LinkedResources))
	for i, lr := range r.LinkedResources {
		resourceSchema, ok := schema.ResourceTypes[lr.TypeName]
		if !ok {
			resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
			return resp
		}

		mp, err := msgpack.Marshal(r.Config, resourceSchema.Body.ImpliedType())
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}

		lrs[i] = &proto6.LinkedResourceConfig{
			TypeName: r.TypeName,
			Config:   &proto6.DynamicValue{Msgpack: mp},
		}
	}

	if len(lrs) > 0 {
		protoReq.LinkedResources = lrs
	}

	protoResp, err := p.client.ValidateActionConfig(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(grpcErr(err))
		return resp
	}
	resp.Diagnostics = resp.Diagnostics.Append(convert.ProtoToDiagnostics(protoResp.Diagnostics))
	return resp
}

// Decode a DynamicValue from either the JSON or MsgPack encoding.
func decodeDynamicValue(v *proto6.DynamicValue, ty cty.Type) (cty.Value, error) {
	// always return a valid value
	var err error
	res := cty.NullVal(ty)
	if v == nil {
		return res, nil
	}

	switch {
	case len(v.Msgpack) > 0:
		res, err = msgpack.Unmarshal(v.Msgpack, ty)
	case len(v.Json) > 0:
		res, err = ctyjson.Unmarshal(v.Json, ty)
	}
	return res, err
}

func clientCapabilitiesToProto(c providers.ClientCapabilities) *proto6.ClientCapabilities {
	return &proto6.ClientCapabilities{
		DeferralAllowed:            c.DeferralAllowed,
		WriteOnlyAttributesAllowed: c.WriteOnlyAttributesAllowed,
	}
}

func linkedResourcePlanDataToProto(schema providers.GetProviderSchemaResponse, linkedResourceSchema []providers.LinkedResourceSchema, lrs []providers.LinkedResourcePlanData) ([]*proto6.PlanAction_Request_LinkedResource, error) {
	protoLinkedResources := make([]*proto6.PlanAction_Request_LinkedResource, 0, len(lrs))

	if len(lrs) != len(linkedResourceSchema) {
		return nil, fmt.Errorf("mismatched number of linked resources: expected %d, got %d", len(linkedResourceSchema), len(lrs))
	}

	for i, lr := range lrs {
		linkedResourceType := linkedResourceSchema[i].TypeName
		// Currently we restrict linked resources to be within the same provider,
		// therefore we can use the schema from the provider to encode and decode the values
		resSchema, ok := schema.ResourceTypes[linkedResourceType]
		if !ok {
			return nil, fmt.Errorf("unknown resource type %q for linked resource #%d", linkedResourceType, i)
		}

		priorStateMP, err := msgpack.Marshal(lr.PriorState, resSchema.Body.ImpliedType())
		if err != nil {
			return nil, fmt.Errorf("failed to marshal prior state for linked resource %q: %w", linkedResourceType, err)
		}

		plannedStateMp, err := msgpack.Marshal(lr.PlannedState, resSchema.Body.ImpliedType())
		if err != nil {
			return nil, fmt.Errorf("failed to marshal planned state for linked resource %q: %w", linkedResourceType, err)
		}

		configMp, err := msgpack.Marshal(lr.Config, resSchema.Body.ImpliedType())
		if err != nil {
			return nil, fmt.Errorf("failed to marshal config for linked resource %q: %w", linkedResourceType, err)
		}

		priorIdentityMp, err := msgpack.Marshal(lr.PriorIdentity, resSchema.Identity.ImpliedType())
		if err != nil {
			return nil, fmt.Errorf("failed to marshal prior identity for linked resource %q: %w", linkedResourceType, err)
		}

		protoLinkedResources = append(protoLinkedResources, &proto6.PlanAction_Request_LinkedResource{
			PriorState:    &proto6.DynamicValue{Msgpack: priorStateMP},
			PlannedState:  &proto6.DynamicValue{Msgpack: plannedStateMp},
			Config:        &proto6.DynamicValue{Msgpack: configMp},
			PriorIdentity: &proto6.ResourceIdentityData{IdentityData: &proto6.DynamicValue{Msgpack: priorIdentityMp}},
		})
	}
	return protoLinkedResources, nil
}

func linkedResourceInvokeDataToProto(schema providers.GetProviderSchemaResponse, linkedResourceSchema []providers.LinkedResourceSchema, lrs []providers.LinkedResourceInvokeData) ([]*proto6.InvokeAction_Request_LinkedResource, error) {
	protoLinkedResources := make([]*proto6.InvokeAction_Request_LinkedResource, 0, len(lrs))

	if len(lrs) != len(linkedResourceSchema) {
		return nil, fmt.Errorf("mismatched number of linked resources: expected %d, got %d", len(linkedResourceSchema), len(lrs))
	}

	for i, lr := range lrs {
		linkedResourceType := linkedResourceSchema[i].TypeName
		// Currently we restrict linked resources to be within the same provider,
		// therefore we can use the schema from the provider to encode and decode the values
		resSchema, ok := schema.ResourceTypes[linkedResourceType]
		if !ok {
			return nil, fmt.Errorf("unknown resource type %q for linked resource #%d", linkedResourceType, i)
		}

		priorStateMP, err := msgpack.Marshal(lr.PriorState, resSchema.Body.ImpliedType())
		if err != nil {
			return nil, fmt.Errorf("failed to marshal prior state for linked resource %q: %w", linkedResourceType, err)
		}

		plannedStateMp, err := msgpack.Marshal(lr.PlannedState, resSchema.Body.ImpliedType())
		if err != nil {
			return nil, fmt.Errorf("failed to marshal planned state for linked resource %q: %w", linkedResourceType, err)
		}

		configMp, err := msgpack.Marshal(lr.Config, resSchema.Body.ImpliedType())
		if err != nil {
			return nil, fmt.Errorf("failed to marshal config for linked resource %q: %w", linkedResourceType, err)
		}

		plannedIdentityMp, err := msgpack.Marshal(lr.PlannedIdentity, resSchema.Identity.ImpliedType())
		if err != nil {
			return nil, fmt.Errorf("failed to marshal planned identity for linked resource %q: %w", linkedResourceType, err)
		}

		protoLinkedResources = append(protoLinkedResources, &proto6.InvokeAction_Request_LinkedResource{
			PriorState:      &proto6.DynamicValue{Msgpack: priorStateMP},
			PlannedState:    &proto6.DynamicValue{Msgpack: plannedStateMp},
			Config:          &proto6.DynamicValue{Msgpack: configMp},
			PlannedIdentity: &proto6.ResourceIdentityData{IdentityData: &proto6.DynamicValue{Msgpack: plannedIdentityMp}},
		})
	}
	return protoLinkedResources, nil
}

func protoToLinkedResourcePlans(schema providers.GetProviderSchemaResponse, linkedResourceSchema []providers.LinkedResourceSchema, lrs []*proto6.PlanAction_Response_LinkedResource) ([]providers.LinkedResourcePlan, error) {

	linkedResources := make([]providers.LinkedResourcePlan, 0, len(lrs))

	if len(lrs) != len(linkedResourceSchema) {
		return nil, fmt.Errorf("mismatched number of linked resources: expected %d, got %d", len(linkedResourceSchema), len(lrs))
	}

	for i, lr := range lrs {
		linkedResourceType := linkedResourceSchema[i].TypeName
		// Currently we restrict linked resources to be within the same provider,
		// therefore we can use the schema from the provider to decode the values
		resSchema, ok := schema.ResourceTypes[linkedResourceType]
		if !ok {
			return nil, fmt.Errorf("unknown resource type %q for linked resource #%d", linkedResourceType, i)
		}

		plannedState, err := decodeDynamicValue(lr.PlannedState, resSchema.Body.ImpliedType())
		if err != nil {
			return nil, fmt.Errorf("failed to decode planned state for linked resource %q: %w", linkedResourceType, err)
		}

		plannedIdentity := cty.NullVal(resSchema.Identity.ImpliedType())
		if lr.PlannedIdentity != nil && lr.PlannedIdentity.IdentityData != nil {
			plannedIdentity, err = decodeDynamicValue(lr.PlannedIdentity.IdentityData, resSchema.Identity.ImpliedType())
			if err != nil {
				return nil, fmt.Errorf("failed to decode prior identity for linked resource %q: %w", linkedResourceType, err)
			}
		}

		linkedResources = append(linkedResources, providers.LinkedResourcePlan{
			PlannedState:    plannedState,
			PlannedIdentity: plannedIdentity,
		})
	}

	return linkedResources, nil
}

func protoToLinkedResourceResults(schema providers.GetProviderSchemaResponse, linkedResourceSchema []providers.LinkedResourceSchema, lrs []*proto6.InvokeAction_Event_Completed_LinkedResource) ([]providers.LinkedResourceResult, error) {

	linkedResources := make([]providers.LinkedResourceResult, 0, len(lrs))

	if len(lrs) != len(linkedResourceSchema) {
		return nil, fmt.Errorf("mismatched number of linked resources: expected %d, got %d", len(linkedResourceSchema), len(lrs))
	}

	for i, lr := range lrs {
		linkedResourceType := linkedResourceSchema[i].TypeName
		// Currently we restrict linked resources to be within the same provider,
		// therefore we can use the schema from the provider to decode the values
		resSchema, ok := schema.ResourceTypes[linkedResourceType]
		if !ok {
			return nil, fmt.Errorf("unknown resource type %q for linked resource #%d", linkedResourceType, i)
		}

		newState, err := decodeDynamicValue(lr.NewState, resSchema.Body.ImpliedType())
		if err != nil {
			return nil, fmt.Errorf("failed to decode planned state for linked resource %q: %w", linkedResourceType, err)
		}

		newIdentity := cty.NullVal(resSchema.Identity.ImpliedType())
		if lr.NewIdentity != nil && lr.NewIdentity.IdentityData != nil {
			newIdentity, err = decodeDynamicValue(lr.NewIdentity.IdentityData, resSchema.Identity.ImpliedType())
			if err != nil {
				return nil, fmt.Errorf("failed to decode prior identity for linked resource %q: %w", linkedResourceType, err)
			}
		}

		linkedResources = append(linkedResources, providers.LinkedResourceResult{
			NewState:        newState,
			NewIdentity:     newIdentity,
			RequiresReplace: lr.RequiresReplace,
		})
	}

	return linkedResources, nil
}
