// Package provider implements the Terraform provider for Latchkey's org
// API: declarative management of OIDC clients, membership grants, auth
// domains, branding, mail templates, tenants, roles, teams and tenant
// grants for one organization.
package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// New builds the provider. Authentication is the org's own confidential
// service client via OAuth2 client_credentials — the same standing the
// org API grants product backends, so Terraform can do exactly what the
// org could do itself and nothing more.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &latchkeyProvider{version: version}
	}
}

type latchkeyProvider struct {
	version string
}

type providerModel struct {
	Issuer       types.String `tfsdk:"issuer"`
	Org          types.String `tfsdk:"org"`
	ClientID     types.String `tfsdk:"client_id"`
	ClientSecret types.String `tfsdk:"client_secret"`
}

func (p *latchkeyProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "latchkey"
	resp.Version = p.version
}

func (p *latchkeyProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manage a Latchkey organization's configuration — OIDC clients, membership grants, auth domains, branding and mail templates — through the org API.",
		Attributes: map[string]schema.Attribute{
			"issuer": schema.StringAttribute{
				Optional:    true,
				Description: "Base URL of the Latchkey issuer (e.g. https://auth.latchkey.id). Falls back to LATCHKEY_ISSUER.",
			},
			"org": schema.StringAttribute{
				Optional:    true,
				Description: "Organization slug this provider manages. Falls back to LATCHKEY_ORG.",
			},
			"client_id": schema.StringAttribute{
				Optional:    true,
				Description: "Confidential service client id (must be tagged to the org, read-write). Falls back to LATCHKEY_CLIENT_ID.",
			},
			"client_secret": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "Service client secret. Falls back to LATCHKEY_CLIENT_SECRET.",
			},
		},
	}
}

func (p *latchkeyProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	get := func(v types.String, env string) string {
		if !v.IsNull() && v.ValueString() != "" {
			return v.ValueString()
		}
		return os.Getenv(env)
	}
	issuer := get(cfg.Issuer, "LATCHKEY_ISSUER")
	org := get(cfg.Org, "LATCHKEY_ORG")
	id := get(cfg.ClientID, "LATCHKEY_CLIENT_ID")
	secret := get(cfg.ClientSecret, "LATCHKEY_CLIENT_SECRET")
	for name, v := range map[string]string{"issuer": issuer, "org": org, "client_id": id, "client_secret": secret} {
		if v == "" {
			resp.Diagnostics.AddError("missing provider configuration",
				name+" is required (set it in the provider block or via LATCHKEY_"+envName(name)+")")
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}
	client := latchkey.New(issuer, org, id, secret)
	resp.ResourceData = client
	resp.DataSourceData = client
}

func envName(attr string) string {
	switch attr {
	case "issuer":
		return "ISSUER"
	case "org":
		return "ORG"
	case "client_id":
		return "CLIENT_ID"
	default:
		return "CLIENT_SECRET"
	}
}

func (p *latchkeyProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		newClientResource,
		newBrandingResource,
		newMailTemplateResource,
		newAuthDomainResource,
		newOrgSandboxResource,
		newGrantResource,
		newApiKeyResource,
		newTenantResource,
		newRoleResource,
		newTeamResource,
		newTeamMemberResource,
		newTenantGrantResource,
		newWebhookResource,
	}
}

func (p *latchkeyProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		newOrgDataSource,
	}
}

// clientFrom unwraps the configured API client for resources/data sources.
func clientFrom(data any) *latchkey.Client {
	c, _ := data.(*latchkey.Client)
	return c
}
