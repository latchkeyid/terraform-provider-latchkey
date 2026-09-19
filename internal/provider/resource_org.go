package provider

import (
	"context"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// latchkey_org — a product organization created through the ESTATE, the
// account above orgs (latchkey gap 21). One apply claims the org for its
// initial owner and mints its default confidential `terraform` client in
// the same transaction; the client's id and secret land in state
// (sensitive) so the estate repo can hand them to the product's own
// `infra/latchkey` — every org manageable as code from its first second.
//
// This resource is the ESTATE's, not an org's: the provider must be
// configured with an estate owner's credentials (the house's own
// confidential client under the `latchkey` org, added as an owner over
// POST /platform/estate/owners). A product's service client is refused.
//
// Creation-time attributes: Latchkey has no org rename and no org
// delete, and ownership transfer is the org owner's own act, so a
// change to slug, display_name or owner_email is refused at plan time
// rather than pretending to replace an org that cannot be destroyed.
// Destroy forgets the org from state and leaves it in place.
type orgResource struct {
	api *latchkey.Client
}

func newOrgResource() resource.Resource { return &orgResource{} }

// orgSlugRe is the live NormalizeSlug rule. The service lowercases, but
// a config that needs lowercasing would never match state — so the
// config must already be the normalized form.
var orgSlugRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)

type orgModel struct {
	ID                    types.String `tfsdk:"id"`
	Slug                  types.String `tfsdk:"slug"`
	DisplayName           types.String `tfsdk:"display_name"`
	OwnerEmail            types.String `tfsdk:"owner_email"`
	OwnerID               types.String `tfsdk:"owner_id"`
	TerraformClientID     types.String `tfsdk:"terraform_client_id"`
	TerraformClientSecret types.String `tfsdk:"terraform_client_secret"`
}

func (r *orgResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org"
}

func (r *orgResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A product organization created through the estate (the account above orgs). Creating it claims the org for `owner_email` and mints its default confidential `terraform` client in the same transaction; the client's id and secret (sensitive, returned once) are what the org's own provider configuration uses. Needs estate-owner credentials. `slug`, `display_name` and `owner_email` are fixed at creation — Latchkey has no org rename or delete, and a change is refused rather than replaced. Destroy only forgets the org from state.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "The org's slug.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"slug": schema.StringAttribute{
				Required:      true,
				Description:   "Org slug (2–32 chars: a–z, 0–9, hyphens not at the ends), already lowercase. Fixed at creation.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.RegexMatches(orgSlugRe, "must be 2–32 lowercase characters: a–z, 0–9, hyphens not at the ends"), stringvalidator.LengthAtLeast(2)},
			},
			"display_name": schema.StringAttribute{
				Required:      true,
				Description:   "The org's display name. Fixed at creation (no org rename).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"owner_email": schema.StringAttribute{
				Required:      true,
				Description:   "The initial owner's email — ensured as an identity and granted `owner`. Fixed at creation; later transfers are the owner's own act.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"owner_id": schema.StringAttribute{
				Computed:      true,
				Description:   "The owner's identity id (their root).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"terraform_client_id": schema.StringAttribute{
				Computed:      true,
				Description:   "The default confidential client's id — the `client_id` for a provider configured on this org.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"terraform_client_secret": schema.StringAttribute{
				Computed:      true,
				Sensitive:     true,
				Description:   "The default client's secret, returned exactly once at creation. Null after import.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *orgResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData != nil {
		r.api = clientFrom(req.ProviderData)
	}
}

// ModifyPlan refuses a replacement outright: Delete cannot remove an
// org, so "replace" would forget it and then fail to re-create the
// same slug (409) — or, for a slug change, silently strand the old org.
// Say so at plan time instead.
func (r *orgResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return // create or destroy — nothing to refuse
	}
	var state, plan orgModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	for _, f := range []struct{ name, was, now string }{
		{"slug", state.Slug.ValueString(), plan.Slug.ValueString()},
		{"display_name", state.DisplayName.ValueString(), plan.DisplayName.ValueString()},
		{"owner_email", state.OwnerEmail.ValueString(), plan.OwnerEmail.ValueString()},
	} {
		if f.was != f.now {
			resp.Diagnostics.AddAttributeError(pathRoot(f.name), "organization attributes are fixed at creation",
				"Latchkey has no org rename or delete, so `"+f.name+"` cannot change from "+f.was+" to "+f.now+
					" — the org would be forgotten from state and could not be re-created. For a new slug, add a second latchkey_org; "+
					"for a new owner, the current owner transfers the org (POST /org/{slug}/transfer).")
		}
	}
}

func (r *orgResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan orgModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	created, err := r.api.CreateOrg(ctx, plan.Slug.ValueString(), plan.DisplayName.ValueString(), plan.OwnerEmail.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("creating organization", err.Error())
		return
	}
	// the service lowercases the address; state keeps the config's
	// spelling (Terraform requires it) and Read never rewrites it
	state := orgModel{
		ID:                    types.StringValue(created.Slug),
		Slug:                  types.StringValue(created.Slug),
		DisplayName:           types.StringValue(created.DisplayName),
		OwnerEmail:            plan.OwnerEmail,
		OwnerID:               types.StringValue(created.OwnerID),
		TerraformClientID:     types.StringValue(created.TerraformClientID),
		TerraformClientSecret: types.StringValue(created.TerraformClientSecret),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *orgResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state orgModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	slug := state.ID.ValueString()
	org, err := r.api.GetEstateOrg(ctx, slug)
	if err != nil {
		resp.Diagnostics.AddError("reading organization", err.Error())
		return
	}
	if org == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	state.Slug = types.StringValue(org.Slug)
	state.DisplayName = types.StringValue(org.DisplayName)
	state.OwnerID = types.StringValue(org.OwnerID)
	state.TerraformClientID = types.StringValue(org.TerraformClientID)
	// the owner may have transferred the org since; the address in state
	// is what was asked for at creation, so only fill it when unknown
	// (import) — a transfer is not drift this resource can act on
	if org.OwnerEmail != "" && (state.OwnerEmail.IsNull() || state.OwnerEmail.ValueString() == "") {
		state.OwnerEmail = types.StringValue(org.OwnerEmail)
	}
	if state.TerraformClientSecret.IsUnknown() {
		state.TerraformClientSecret = types.StringNull() // shown once, at creation
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *orgResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// every argument RequiresReplace and ModifyPlan refuses the
	// replacement, so Update never runs; the interface wants it
	var plan orgModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *orgResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	// Latchkey has no org delete: the org stays; only state forgets it.
	resp.Diagnostics.AddWarning("organization left in place",
		"Latchkey does not delete organizations; the org still exists upstream and importing this resource by slug adopts it (without the client secret).")
}

func (r *orgResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("terraform_client_secret"), types.StringNull())...)
}
