package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// latchkey_tenant_grant — one identity's stored grant on a tenant
// namespace ({org}/{tenant}). Org-level roles are deliberately out of
// reach here (they stay behind the owner-gated invite flow, so a
// service client can never mint an org owner). The grant endpoint is a
// convergent upsert, so level and roles update in place; revoke is the
// destroy and a later re-grant would re-establish the same stream.
type tenantGrantResource struct {
	api *latchkey.Client
}

func newTenantGrantResource() resource.Resource { return &tenantGrantResource{} }

type tenantGrantModel struct {
	ID         types.String `tfsdk:"id"`
	Tenant     types.String `tfsdk:"tenant"`
	Email      types.String `tfsdk:"email"`
	Level      types.String `tfsdk:"level"`
	Roles      types.List   `tfsdk:"roles"`
	IdentityID types.String `tfsdk:"identity_id"`
}

func (r *tenantGrantResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_tenant_grant"
}

func (r *tenantGrantResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "One identity's grant on a tenant ({org}/{tenant} namespace). The identity is created " +
			"if the email is new; the grant lands in the member's token at their next sign-in or refresh. " +
			"The tenant must exist as an object (latchkey_tenant) — grants on free-form sub-namespaces " +
			"have no read-back and stay outside Terraform.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, Description: "tenant/email.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"tenant": schema.StringAttribute{
				Required:      true,
				Description:   "Slug of the tenant the grant scopes to.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"email": schema.StringAttribute{
				Required:      true,
				Description:   "The grantee's email.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"level": schema.StringAttribute{
				Required:    true,
				Description: "admin, member or viewer. Tenant admins manage the tenant's SSO, keys and members.",
				Validators:  []validator.String{stringvalidator.OneOf("admin", "member", "viewer")},
			},
			"roles": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Registry roles carried by the grant — each must be defined (latchkey_role).",
			},
			"identity_id": schema.StringAttribute{
				Computed:      true,
				Description:   "The grantee's root identity id, resolved at create.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *tenantGrantResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData != nil {
		r.api = clientFrom(req.ProviderData)
	}
}

func (r *tenantGrantResource) namespace(m tenantGrantModel) string {
	return r.api.Org + "/" + m.Tenant.ValueString()
}

func (r *tenantGrantResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan tenantGrantModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := r.api.GrantNamespace(ctx, plan.Email.ValueString(), r.namespace(plan),
		plan.Level.ValueString(), stringList(ctx, plan.Roles))
	if err != nil {
		resp.Diagnostics.AddError("granting tenant access", err.Error())
		return
	}
	plan.IdentityID = types.StringValue(id)
	plan.ID = types.StringValue(plan.Tenant.ValueString() + "/" + plan.Email.ValueString())
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *tenantGrantResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state tenantGrantModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	members, err := r.api.TenantMembers(ctx, state.Tenant.ValueString())
	if err != nil {
		// an archived-then-forgotten tenant answers 404 — the grant is
		// unreachable, so it is gone for terraform's purposes
		if latchkey.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("reading tenant members", err.Error())
		return
	}
	for _, m := range members {
		if m.IdentityID != state.IdentityID.ValueString() {
			continue
		}
		state.Level = types.StringValue(m.Level)
		state.Roles = stringListValue(ctx, m.Roles, state.Roles)
		resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
		return
	}
	// revoked or gone — recreate on next apply
	resp.State.RemoveResource(ctx)
}

func (r *tenantGrantResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state tenantGrantModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, err := r.api.GrantNamespace(ctx, plan.Email.ValueString(), r.namespace(plan),
		plan.Level.ValueString(), stringList(ctx, plan.Roles)); err != nil {
		resp.Diagnostics.AddError("updating tenant grant", err.Error())
		return
	}
	plan.ID, plan.IdentityID = state.ID, state.IdentityID
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *tenantGrantResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state tenantGrantModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.api.RevokeNamespaceGrant(ctx, state.Email.ValueString(), r.namespace(state)); err != nil && !latchkey.IsNotFound(err) {
		resp.Diagnostics.AddError("revoking tenant grant", err.Error())
	}
}
