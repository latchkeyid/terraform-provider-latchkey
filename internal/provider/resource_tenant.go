package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// latchkey_tenant — one tenant under the org: the anchor SSO
// connections, API keys, teams and grants hang off. Creation is
// claim-style (idempotent), so apply converges the tenant onto the
// config even when the slug already exists. Destroy archives — one-way,
// and the slug is never reusable afterwards.
type tenantResource struct {
	api *latchkey.Client
}

func newTenantResource() resource.Resource { return &tenantResource{} }

type tenantModel struct {
	ID          types.String `tfsdk:"id"`
	Slug        types.String `tfsdk:"slug"`
	DisplayName types.String `tfsdk:"display_name"`
	Parent      types.String `tfsdk:"parent"`
	SsoRequired types.Bool   `tfsdk:"sso_required"`
}

func (r *tenantResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_tenant"
}

func (r *tenantResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A tenant under the org — the unit SSO connections, teams, API keys and grants attach to. " +
			"An enterprise is just a tenant other tenants point at via `parent` (one level deep). " +
			"Destroy archives the tenant permanently: the slug can never be claimed again.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, Description: "The tenant slug.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"slug": schema.StringAttribute{
				Required: true,
				Description: "Org-wide-unique tenant slug (lowercase letters, digits, `._-`, max 63 chars). " +
					"Immutable — and burned forever on destroy.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"display_name": schema.StringAttribute{
				Required:    true,
				Description: "Human name for the tenant.",
			},
			"parent": schema.StringAttribute{
				Optional: true,
				Description: "Slug of the enterprise tenant this one belongs to. The parent must be a root " +
					"tenant (nesting is one level deep). Joining or leaving an enterprise is one pointer write.",
			},
			"sso_required": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Require a fresh SSO authentication before this tenant's claims are minted. " +
					"Cascades strictest-ancestor-wins: a child cannot opt out of its enterprise's SSO.",
			},
		},
	}
}

func (r *tenantResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData != nil {
		r.api = clientFrom(req.ProviderData)
	}
}

// converge drives the live tenant to the plan: claim never updates an
// existing tenant, so create and update share the same setter walk.
func (r *tenantResource) converge(ctx context.Context, plan tenantModel, diags *diag.Diagnostics) bool {
	slug := plan.Slug.ValueString()
	live, err := r.api.GetTenant(ctx, slug)
	if err != nil {
		diags.AddError("reading tenant back", err.Error())
		return false
	}
	if live.DisplayName != plan.DisplayName.ValueString() {
		if err := r.api.RenameTenant(ctx, slug, plan.DisplayName.ValueString()); err != nil {
			diags.AddError("renaming tenant", err.Error())
			return false
		}
	}
	if want := plan.Parent.ValueString(); want != live.Parent {
		if want == "" {
			err = r.api.ClearTenantParent(ctx, slug)
		} else {
			err = r.api.SetTenantParent(ctx, slug, want)
		}
		if err != nil {
			diags.AddError("setting tenant parent", err.Error())
			return false
		}
	}
	if plan.SsoRequired.ValueBool() != live.SsoRequired {
		if err := r.api.SetTenantSsoRequired(ctx, slug, plan.SsoRequired.ValueBool()); err != nil {
			diags.AddError("setting sso_required", err.Error())
			return false
		}
	}
	return true
}

func (r *tenantResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan tenantModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.api.ClaimTenant(ctx, plan.Slug.ValueString(), plan.DisplayName.ValueString(), plan.Parent.ValueString()); err != nil {
		resp.Diagnostics.AddError("claiming tenant", err.Error())
		return
	}
	// claim converges to a no-op on an existing tenant without touching
	// its config — walk the setters so apply means what the config says
	if !r.converge(ctx, plan, &resp.Diagnostics) {
		return
	}
	plan.ID = plan.Slug
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *tenantResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state tenantModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	t, err := r.api.GetTenant(ctx, state.Slug.ValueString())
	if err != nil {
		if latchkey.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("reading tenant", err.Error())
		return
	}
	// archived is gone for terraform's purposes — there is no unarchive,
	// and every config write is refused
	if t.Status == "archived" {
		resp.State.RemoveResource(ctx)
		return
	}
	state.ID = state.Slug
	state.DisplayName = types.StringValue(t.DisplayName)
	state.Parent = stringOrNull(t.Parent, state.Parent)
	state.SsoRequired = types.BoolValue(t.SsoRequired)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *tenantResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan tenantModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !r.converge(ctx, plan, &resp.Diagnostics) {
		return
	}
	plan.ID = plan.Slug
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *tenantResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state tenantModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.api.ArchiveTenant(ctx, state.Slug.ValueString()); err != nil && !latchkey.IsNotFound(err) {
		resp.Diagnostics.AddError("archiving tenant", err.Error())
	}
}
