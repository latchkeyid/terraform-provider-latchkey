package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// latchkey_team — a grant target under a tenant: granting to the team
// confers its bindings on every member at token mint. Terraform-created
// teams are always hand-managed; IdP-synced (SCIM) teams accept only the
// identity provider as roster writer and cannot be created here.
// Membership is people-churn, managed one at a time — see
// latchkey_team_member. Destroy soft-deletes: the slug stays retired
// forever.
type teamResource struct {
	api *latchkey.Client
}

func newTeamResource() resource.Resource { return &teamResource{} }

type teamModel struct {
	ID          types.String `tfsdk:"id"`
	Slug        types.String `tfsdk:"slug"`
	Tenant      types.String `tfsdk:"tenant"`
	DisplayName types.String `tfsdk:"display_name"`
	Bindings    types.List   `tfsdk:"bindings"`
}

type teamBindingModel struct {
	Ns    types.String `tfsdk:"ns"`
	Level types.String `tfsdk:"level"`
	Roles types.List   `tfsdk:"roles"`
}

var teamBindingAttrTypes = map[string]attr.Type{
	"ns":    types.StringType,
	"level": types.StringType,
	"roles": types.ListType{ElemType: types.StringType},
}

func (r *teamResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_team"
}

func (r *teamResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A team under a tenant — a first-class grant target: bindings confer namespaces, levels " +
			"and roles on every member at token mint. Members are managed with latchkey_team_member. " +
			"Destroy soft-deletes the team and retires its slug forever.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, Description: "The team slug.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"slug": schema.StringAttribute{
				Required: true,
				Description: "Org-wide-unique team slug (lowercase letters, digits, `._-`, max 63 chars). " +
					"Immutable — and retired forever on destroy.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"tenant": schema.StringAttribute{
				Required:      true,
				Description:   "Slug of the tenant that owns the team. A team cannot move between tenants.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"display_name": schema.StringAttribute{
				Required:    true,
				Description: "Human name for the team.",
			},
			"bindings": schema.ListNestedAttribute{
				Optional: true,
				Description: "What membership confers, replaced as a whole list. Each binding names a " +
					"sub-namespace of the org, an access level, and optionally registry roles.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"ns": schema.StringAttribute{
							Required:    true,
							Description: "Namespace the binding confers, `{org}/{tenant}` shaped.",
						},
						"level": schema.StringAttribute{
							Required:    true,
							Description: "admin, member or viewer.",
							Validators:  []validator.String{stringvalidator.OneOf("admin", "member", "viewer")},
						},
						"roles": schema.ListAttribute{
							ElementType: types.StringType,
							Optional:    true,
							Description: "Registry roles conferred alongside the level — each must be defined (latchkey_role).",
						},
					},
				},
			},
		},
	}
}

func (r *teamResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData != nil {
		r.api = clientFrom(req.ProviderData)
	}
}

// bindingsOut unwraps the plan's binding list for the API.
func bindingsOut(ctx context.Context, l types.List) []latchkey.TeamBinding {
	if l.IsNull() || l.IsUnknown() {
		return nil
	}
	var models []teamBindingModel
	l.ElementsAs(ctx, &models, false)
	out := make([]latchkey.TeamBinding, 0, len(models))
	for _, m := range models {
		out = append(out, latchkey.TeamBinding{
			Ns:    m.Ns.ValueString(),
			Level: m.Level.ValueString(),
			Roles: stringList(ctx, m.Roles),
		})
	}
	return out
}

// bindingsValue wraps the service's binding list, preserving null when
// the prior value was null and the team has none — the list-flap rule.
func bindingsValue(ctx context.Context, v []latchkey.TeamBinding, prior types.List) types.List {
	if len(v) == 0 && prior.IsNull() {
		return types.ListNull(types.ObjectType{AttrTypes: teamBindingAttrTypes})
	}
	models := make([]teamBindingModel, 0, len(v))
	for _, b := range v {
		roles := types.ListNull(types.StringType)
		if len(b.Roles) > 0 {
			roles, _ = types.ListValueFrom(ctx, types.StringType, b.Roles)
		}
		models = append(models, teamBindingModel{
			Ns:    types.StringValue(b.Ns),
			Level: types.StringValue(b.Level),
			Roles: roles,
		})
	}
	out, _ := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: teamBindingAttrTypes}, models)
	return out
}

func (r *teamResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan teamModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.api.CreateTeam(ctx, plan.Slug.ValueString(), plan.Tenant.ValueString(), plan.DisplayName.ValueString()); err != nil {
		resp.Diagnostics.AddError("creating team", err.Error())
		return
	}
	if b := bindingsOut(ctx, plan.Bindings); len(b) > 0 {
		if err := r.api.SetTeamBindings(ctx, plan.Slug.ValueString(), b); err != nil {
			resp.Diagnostics.AddError("setting team bindings", err.Error())
			return
		}
	}
	plan.ID = plan.Slug
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *teamResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state teamModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	t, err := r.api.GetTeam(ctx, state.Slug.ValueString())
	if err != nil {
		// deleted teams answer 404 like never-created ones
		if latchkey.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("reading team", err.Error())
		return
	}
	state.ID = state.Slug
	state.Tenant = types.StringValue(t.Tenant)
	state.DisplayName = types.StringValue(t.DisplayName)
	state.Bindings = bindingsValue(ctx, t.Bindings, state.Bindings)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *teamResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state teamModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !plan.DisplayName.Equal(state.DisplayName) {
		// the create endpoint upserts on display name — that IS rename
		if err := r.api.CreateTeam(ctx, plan.Slug.ValueString(), plan.Tenant.ValueString(), plan.DisplayName.ValueString()); err != nil {
			resp.Diagnostics.AddError("renaming team", err.Error())
			return
		}
	}
	if !plan.Bindings.Equal(state.Bindings) {
		if err := r.api.SetTeamBindings(ctx, plan.Slug.ValueString(), bindingsOut(ctx, plan.Bindings)); err != nil {
			resp.Diagnostics.AddError("setting team bindings", err.Error())
			return
		}
	}
	plan.ID = plan.Slug
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *teamResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state teamModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.api.DeleteTeam(ctx, state.Slug.ValueString()); err != nil && !latchkey.IsNotFound(err) {
		resp.Diagnostics.AddError("deleting team", err.Error())
	}
}
