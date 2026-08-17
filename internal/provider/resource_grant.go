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

// latchkey_grant — one identity's membership in the org. Passwordless
// makes acceptance implicit: the identity is ensured at grant time and
// the person signs in with that email to arrive as a member.
type grantResource struct {
	api *latchkey.Client
}

func newGrantResource() resource.Resource { return &grantResource{} }

type grantModel struct {
	ID    types.String `tfsdk:"id"`
	Email types.String `tfsdk:"email"`
	Role  types.String `tfsdk:"role"`
}

func (r *grantResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_grant"
}

func (r *grantResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A membership grant in the org. The identity is created if the email is new; sign-in with that email arrives as a member.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, Description: "The email."},
			"email": schema.StringAttribute{
				Required:      true,
				Description:   "The member's email.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"role": schema.StringAttribute{
				Required:    true,
				Description: "owner, member or viewer.",
				Validators:  []validator.String{stringvalidator.OneOf("owner", "member", "viewer")},
			},
		},
	}
}

func (r *grantResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData != nil {
		r.api = clientFrom(req.ProviderData)
	}
}

func (r *grantResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan grantModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.api.Grant(ctx, plan.Email.ValueString(), r.api.Org, plan.Role.ValueString()); err != nil {
		resp.Diagnostics.AddError("granting membership", err.Error())
		return
	}
	plan.ID = plan.Email
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *grantResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state grantModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	m, err := r.api.GetMember(ctx, state.Email.ValueString())
	if err != nil {
		if latchkey.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("reading membership", err.Error())
		return
	}
	state.ID = state.Email
	state.Role = types.StringValue(m.Role)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *grantResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan grantModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.api.Grant(ctx, plan.Email.ValueString(), r.api.Org, plan.Role.ValueString()); err != nil {
		resp.Diagnostics.AddError("updating membership role", err.Error())
		return
	}
	plan.ID = plan.Email
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *grantResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state grantModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.api.RevokeGrant(ctx, state.Email.ValueString(), r.api.Org); err != nil && !latchkey.IsNotFound(err) {
		resp.Diagnostics.AddError("revoking membership", err.Error())
	}
}
