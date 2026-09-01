package provider

import (
	"context"
	"slices"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// latchkey_team_member — one identity's place on a hand-managed team.
// The write path takes an email (the identity is ensured if new); the
// team object echoes only root identity ids back, so the resolved id is
// captured at create and drives refresh and removal. IdP-managed (SCIM)
// teams refuse membership writes — the identity provider owns those
// rosters.
type teamMemberResource struct {
	api *latchkey.Client
}

func newTeamMemberResource() resource.Resource { return &teamMemberResource{} }

type teamMemberModel struct {
	ID         types.String `tfsdk:"id"`
	Team       types.String `tfsdk:"team"`
	Email      types.String `tfsdk:"email"`
	IdentityID types.String `tfsdk:"identity_id"`
}

func (r *teamMemberResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_team_member"
}

func (r *teamMemberResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "One identity's membership on a hand-managed team. The identity is created if the email " +
			"is new; the team's bindings confer on the member at their next token mint.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, Description: "team/identity id.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"team": schema.StringAttribute{
				Required:      true,
				Description:   "The team's slug.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"email": schema.StringAttribute{
				Required:      true,
				Description:   "The member's email.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"identity_id": schema.StringAttribute{
				Computed:      true,
				Description:   "The member's root identity id, resolved at create.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *teamMemberResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData != nil {
		r.api = clientFrom(req.ProviderData)
	}
}

func (r *teamMemberResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan teamMemberModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := r.api.AddTeamMember(ctx, plan.Team.ValueString(), plan.Email.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("adding team member", err.Error())
		return
	}
	plan.IdentityID = types.StringValue(id)
	plan.ID = types.StringValue(plan.Team.ValueString() + "/" + id)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *teamMemberResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state teamMemberModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	t, err := r.api.GetTeam(ctx, state.Team.ValueString())
	if err != nil {
		if latchkey.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("reading team", err.Error())
		return
	}
	if !slices.Contains(t.Members, state.IdentityID.ValueString()) {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

// Update never runs — every attribute carries RequiresReplace. Keep the
// computed identity from prior state if the framework routes here.
func (r *teamMemberResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state teamMemberModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID, plan.IdentityID = state.ID, state.IdentityID
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *teamMemberResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state teamMemberModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.api.RemoveTeamMember(ctx, state.Team.ValueString(), state.IdentityID.ValueString()); err != nil && !latchkey.IsNotFound(err) {
		resp.Diagnostics.AddError("removing team member", err.Error())
	}
}
