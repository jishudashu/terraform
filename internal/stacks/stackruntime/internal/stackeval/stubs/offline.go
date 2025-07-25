// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package stubs

import (
	"github.com/hashicorp/terraform/internal/providers"
	"github.com/hashicorp/terraform/internal/tfdiags"
)

// offlineProvider is a stub provider that is used in place of a provider that
// is not configured  and should never be configured by the current Terraform
// configuration.
//
// The only functionality that should be called on an offlineProvider are
// provider function calls and move resource state.
//
// For everything else, Stacks should have provided a pre-configured provider
// that should be used instead.
type offlineProvider struct {
	unconfiguredClient providers.Interface
}

func OfflineProvider(unconfiguredClient providers.Interface) providers.Interface {
	return &offlineProvider{
		unconfiguredClient: unconfiguredClient,
	}
}

func (o *offlineProvider) GetProviderSchema() providers.GetProviderSchemaResponse {
	// We do actually use the schema to work out which functions are available
	// and whether cross-resource moves are even supported.
	return o.unconfiguredClient.GetProviderSchema()
}

func (o *offlineProvider) GetResourceIdentitySchemas() providers.GetResourceIdentitySchemasResponse {
	return o.unconfiguredClient.GetResourceIdentitySchemas()
}

func (o *offlineProvider) ValidateProviderConfig(_ providers.ValidateProviderConfigRequest) providers.ValidateProviderConfigResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called ValidateProviderConfig on an unconfigured provider",
		"Cannot validate provider configuration because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.ValidateProviderConfigResponse{
		Diagnostics: diags,
	}
}

func (o *offlineProvider) ValidateResourceConfig(_ providers.ValidateResourceConfigRequest) providers.ValidateResourceConfigResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called ValidateResourceConfig on an unconfigured provider",
		"Cannot validate resource configuration because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.ValidateResourceConfigResponse{
		Diagnostics: diags,
	}
}

func (o *offlineProvider) ValidateDataResourceConfig(_ providers.ValidateDataResourceConfigRequest) providers.ValidateDataResourceConfigResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called ValidateDataResourceConfig on an unconfigured provider",
		"Cannot validate data source configuration because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.ValidateDataResourceConfigResponse{
		Diagnostics: diags,
	}
}

// ValidateEphemeralResourceConfig implements providers.Interface.
func (p *offlineProvider) ValidateEphemeralResourceConfig(providers.ValidateEphemeralResourceConfigRequest) providers.ValidateEphemeralResourceConfigResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called ValidateEphemeralResourceConfig on an unconfigured provider",
		"Cannot validate this resource config because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.ValidateEphemeralResourceConfigResponse{
		Diagnostics: diags,
	}
}

// ValidateListResourceConfig implements providers.Interface.
func (p *offlineProvider) ValidateListResourceConfig(providers.ValidateListResourceConfigRequest) providers.ValidateListResourceConfigResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called ValidateListResourceConfig on an unconfigured provider",
		"Cannot validate this resource config because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.ValidateListResourceConfigResponse{
		Diagnostics: diags,
	}
}

func (o *offlineProvider) UpgradeResourceState(_ providers.UpgradeResourceStateRequest) providers.UpgradeResourceStateResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called UpgradeResourceState on an unconfigured provider",
		"Cannot upgrade the state of this resource because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.UpgradeResourceStateResponse{
		Diagnostics: diags,
	}
}

func (o *offlineProvider) UpgradeResourceIdentity(_ providers.UpgradeResourceIdentityRequest) providers.UpgradeResourceIdentityResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called UpgradeResourceIdentity on an unconfigured provider",
		"Cannot upgrade the state of this resource because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.UpgradeResourceIdentityResponse{
		Diagnostics: diags,
	}
}

func (o *offlineProvider) ConfigureProvider(_ providers.ConfigureProviderRequest) providers.ConfigureProviderResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called ConfigureProvider on an unconfigured provider",
		"Cannot configure this provider because it is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.ConfigureProviderResponse{
		Diagnostics: diags,
	}
}

func (o *offlineProvider) Stop() error {
	// pass the stop call to the underlying unconfigured client
	return o.unconfiguredClient.Stop()
}

func (o *offlineProvider) ReadResource(_ providers.ReadResourceRequest) providers.ReadResourceResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called ReadResource on an unconfigured provider",
		"Cannot read from this resource because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.ReadResourceResponse{
		Diagnostics: diags,
	}
}

func (o *offlineProvider) PlanResourceChange(_ providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called PlanResourceChange on an unconfigured provider",
		"Cannot plan changes to this resource because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.PlanResourceChangeResponse{
		Diagnostics: diags,
	}
}

func (o *offlineProvider) ApplyResourceChange(_ providers.ApplyResourceChangeRequest) providers.ApplyResourceChangeResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called ApplyResourceChange on an unconfigured provider",
		"Cannot apply changes to this resource because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.ApplyResourceChangeResponse{
		Diagnostics: diags,
	}
}

func (o *offlineProvider) ImportResourceState(_ providers.ImportResourceStateRequest) providers.ImportResourceStateResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called ImportResourceState on an unconfigured provider",
		"Cannot import an existing object into this resource because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.ImportResourceStateResponse{
		Diagnostics: diags,
	}
}

func (o *offlineProvider) MoveResourceState(request providers.MoveResourceStateRequest) providers.MoveResourceStateResponse {
	return o.unconfiguredClient.MoveResourceState(request)
}

func (o *offlineProvider) ReadDataSource(_ providers.ReadDataSourceRequest) providers.ReadDataSourceResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called ReadDataSource on an unconfigured provider",
		"Cannot read from this data source because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.ReadDataSourceResponse{
		Diagnostics: diags,
	}
}

// OpenEphemeralResource implements providers.Interface.
func (u *offlineProvider) OpenEphemeralResource(providers.OpenEphemeralResourceRequest) providers.OpenEphemeralResourceResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called OpenEphemeralResource on an unconfigured provider",
		"Cannot open this resource instance because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.OpenEphemeralResourceResponse{
		Diagnostics: diags,
	}
}

// RenewEphemeralResource implements providers.Interface.
func (u *offlineProvider) RenewEphemeralResource(providers.RenewEphemeralResourceRequest) providers.RenewEphemeralResourceResponse {
	// We don't have anything to do here because OpenEphemeralResource didn't really
	// actually "open" anything.
	return providers.RenewEphemeralResourceResponse{}
}

// CloseEphemeralResource implements providers.Interface.
func (u *offlineProvider) CloseEphemeralResource(providers.CloseEphemeralResourceRequest) providers.CloseEphemeralResourceResponse {
	// We don't have anything to do here because OpenEphemeralResource didn't really
	// actually "open" anything.
	return providers.CloseEphemeralResourceResponse{}
}

func (o *offlineProvider) CallFunction(request providers.CallFunctionRequest) providers.CallFunctionResponse {
	return o.unconfiguredClient.CallFunction(request)
}

func (o *offlineProvider) ListResource(providers.ListResourceRequest) providers.ListResourceResponse {
	var resp providers.ListResourceResponse
	resp.Diagnostics = resp.Diagnostics.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called ListResource on an unconfigured provider",
		"Cannot list this resource because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return resp
}

// ValidateStateStoreConfig implements providers.Interface.
func (o *offlineProvider) ValidateStateStoreConfig(providers.ValidateStateStoreConfigRequest) providers.ValidateStateStoreConfigResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called ValidateStateStoreConfig on an unconfigured provider",
		"Cannot validate state store because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.ValidateStateStoreConfigResponse{
		Diagnostics: diags,
	}
}

// ConfigureStateStore implements providers.Interface.
func (o *offlineProvider) ConfigureStateStore(providers.ConfigureStateStoreRequest) providers.ConfigureStateStoreResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called ConfigureStateStore on an unconfigured provider",
		"Cannot configure state store because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.ConfigureStateStoreResponse{
		Diagnostics: diags,
	}
}

// GetStates implements providers.Interface.
func (o *offlineProvider) GetStates(providers.GetStatesRequest) providers.GetStatesResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called GetStates on an unconfigured provider",
		"Cannot list states managed by this state store because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.GetStatesResponse{
		Diagnostics: diags,
	}
}

// DeleteState implements providers.Interface.
func (o *offlineProvider) DeleteState(providers.DeleteStateRequest) providers.DeleteStateResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called DeleteState on an unconfigured provider",
		"Cannot use this state store to delete a state because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.DeleteStateResponse{
		Diagnostics: diags,
	}
}

// PlanAction implements providers.Interface.
func (o *offlineProvider) PlanAction(request providers.PlanActionRequest) providers.PlanActionResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called PlanAction on an unconfigured provider",
		"Cannot plan this action because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.PlanActionResponse{
		Diagnostics: diags,
	}
}

// InvokeAction implements providers.Interface.
func (o *offlineProvider) InvokeAction(request providers.InvokeActionRequest) providers.InvokeActionResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called InvokeAction on an unconfigured provider",
		"Cannot invoke this action because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.InvokeActionResponse{
		Diagnostics: diags,
	}
}

// InvokeAction implements providers.Interface.
func (o *offlineProvider) ValidateActionConfig(request providers.ValidateActionConfigRequest) providers.ValidateActionConfigResponse {
	var diags tfdiags.Diagnostics
	diags = diags.Append(tfdiags.AttributeValue(
		tfdiags.Error,
		"Called ValidateActionConfig on an unconfigured provider",
		"Cannot invoke this action because this provider is not configured. This is a bug in Terraform - please report it.",
		nil, // nil attribute path means the overall configuration block
	))
	return providers.ValidateActionConfigResponse{
		Diagnostics: diags,
	}
}

func (o *offlineProvider) Close() error {
	// pass the close call to the underlying unconfigured client
	return o.unconfiguredClient.Close()
}
