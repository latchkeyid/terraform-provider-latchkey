package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// latchkey_role — one definition in the org's role registry: the open,
// org-defined vocabulary ("resident", "committee") grants and team
// bindings reference. The define endpoint is a true upsert, so create
// and update are the same call. Destroy retires: the role confers
// nothing and refuses new grants, but existing grants keep carrying the
// string, and redefining the slug un-retires it.
type roleResource struct {
	api *latchkey.Client
}

func newRoleResource() resource.Resource { return &roleResource{} }

type roleModel struct {
	ID           types.String `tfsdk:"id"`
	Slug         types.String `tfsdk:"slug"`
	DisplayName  types.String `tfsdk:"display_name"`
	BaseLevel    types.String `tfsdk:"base_level"`
	Capabilities types.List   `tfsdk:"capabilities"`
}

func (r *roleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_role"
}

func (r *roleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A role definition in the org's registry — the product-defined vocabulary tenant grants " +
			"and team bindings reference. `base_level` makes granting the role confer that access level at " +
			"mint (GitHub's custom-role inheritance); editing a definition re-levels every holder at their " +
			"next token refresh. Destroy retires the role: it confers nothing and new grants refuse it.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, Description: "The role slug.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"slug": schema.StringAttribute{
				Required:      true,
				Description:   "Org-wide-unique role slug (lowercase letters, digits, `._-`, max 63 chars).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"display_name": schema.StringAttribute{
				Required:    true,
				Description: "Human name for the role.",
			},
			"base_level": schema.StringAttribute{
				Optional: true,
				Description: "Access level the role confers on its holders at token mint: admin, member or " +
					"viewer. Unset confers nothing — the role is a pure product-vocabulary string. " +
					"owner is deliberately not grantable this way.",
				Validators: []validator.String{stringvalidator.OneOf("admin", "member", "viewer")},
			},
			"capabilities": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Reserved capabilities latchkey itself interprets. `manages_tenant` (the legacy " +
					"spelling of `base_level = \"admin\"`) lets holders manage the tenant's SSO, keys and members.",
				Validators: []validator.List{listvalidator.ValueStringsAre(stringvalidator.OneOf("manages_tenant"))},
			},
		},
	}
}

func (r *roleResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData != nil {
		r.api = clientFrom(req.ProviderData)
	}
}

func (r *roleResource) define(ctx context.Context, plan roleModel) error {
	return r.api.DefineRole(ctx, latchkey.Role{
		Slug:         plan.Slug.ValueString(),
		DisplayName:  plan.DisplayName.ValueString(),
		BaseLevel:    plan.BaseLevel.ValueString(),
		Capabilities: stringList(ctx, plan.Capabilities),
	})
}

func (r *roleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan roleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.define(ctx, plan); err != nil {
		resp.Diagnostics.AddError("defining role", err.Error())
		return
	}
	plan.ID = plan.Slug
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *roleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state roleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	role, err := r.api.GetRole(ctx, state.Slug.ValueString())
	if err != nil {
		// retired roles vanish from the registry list — same as gone
		if latchkey.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("reading role", err.Error())
		return
	}
	state.ID = state.Slug
	state.DisplayName = types.StringValue(role.DisplayName)
	state.BaseLevel = stringOrNull(role.BaseLevel, state.BaseLevel)
	state.Capabilities = stringListValue(ctx, role.Capabilities, state.Capabilities)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *roleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan roleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.define(ctx, plan); err != nil {
		resp.Diagnostics.AddError("updating role", err.Error())
		return
	}
	plan.ID = plan.Slug
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *roleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state roleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.api.RetireRole(ctx, state.Slug.ValueString()); err != nil && !latchkey.IsNotFound(err) {
		resp.Diagnostics.AddError("retiring role", err.Error())
	}
}
