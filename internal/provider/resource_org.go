package provider

import (
	"context"
	"net/http"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

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
//
// ADR 012 (orgs follow anythink tenants): a production org is BOUND to
// one tenant of the house org — the customer it serves. `tenant` names
// an existing house tenant (key, ref or slug — one owner_email
// administers) and `new_tenant` asks for a new one with the owner its
// admin; both are sent to POST /platform/orgs at creation. The binding
// reads back from GET /org/{slug} into tenant_key / tenant_ref /
// tenant_slug. `tenant` keeps the spelling the config used (Terraform
// requires it), so a ref or slug that resolves to the bound tenant is
// no diff. A rebind is a platform staff act, refused at plan time; an
// unbound org (created before the invariant) may be bound in place.
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
	Tenant                types.String `tfsdk:"tenant"`
	NewTenant             types.Object `tfsdk:"new_tenant"`
	TenantKey             types.String `tfsdk:"tenant_key"`
	TenantRef             types.String `tfsdk:"tenant_ref"`
	TenantSlug            types.String `tfsdk:"tenant_slug"`
}

type newTenantModel struct {
	DisplayName types.String `tfsdk:"display_name"`
	Slug        types.String `tfsdk:"slug"`
}

var newTenantAttrTypes = map[string]attr.Type{
	"display_name": types.StringType,
	"slug":         types.StringType,
}

// tenantNames is every name the bound tenant answers to in state: a
// configured `tenant` matching any of them is the binding, not a change.
func (m orgModel) tenantNames() []string {
	var out []string
	for _, v := range []types.String{m.TenantKey, m.TenantRef, m.TenantSlug} {
		if !v.IsNull() && !v.IsUnknown() && v.ValueString() != "" {
			out = append(out, v.ValueString())
		}
	}
	return out
}

func (m orgModel) bound() bool {
	return !m.TenantKey.IsNull() && !m.TenantKey.IsUnknown() && m.TenantKey.ValueString() != ""
}

// setTenant fills the computed binding from the server's view; nil is
// unbound.
func (m *orgModel) setTenant(t *latchkey.BoundTenant) {
	if t == nil || t.Key == "" {
		m.TenantKey, m.TenantRef, m.TenantSlug = types.StringNull(), types.StringNull(), types.StringNull()
		return
	}
	m.TenantKey = types.StringValue(t.Key)
	m.TenantRef = stringOrNull(t.Ref, types.StringNull())
	m.TenantSlug = stringOrNull(t.Slug, types.StringNull())
}

func (m *orgModel) unknownTenantToNull() {
	for _, v := range []*types.String{&m.TenantKey, &m.TenantRef, &m.TenantSlug} {
		if v.IsUnknown() {
			*v = types.StringNull()
		}
	}
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
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
			"tenant": schema.StringAttribute{
				Optional: true,
				Description: "The anythink (house) tenant the org is bound to at creation — its key, ref or slug; `owner_email` must administer it. " +
					"Exclusive with `new_tenant`; once Latchkey requires bound orgs, one of the two is needed to create. Kept as written: a ref or slug " +
					"that resolves to the bound tenant is no diff. Setting it on an unbound org binds it in place; changing it on a bound org is refused " +
					"(a rebind is a platform staff act).",
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"new_tenant": schema.SingleNestedAttribute{
				Optional: true,
				Description: "Create a new anythink tenant for the org at creation, with `owner_email` its admin. Exclusive with `tenant`. " +
					"Read at creation only — afterwards the binding is in `tenant_key`.",
				Attributes: map[string]schema.Attribute{
					"display_name": schema.StringAttribute{Optional: true, Description: "The tenant's display name (default: the org's)."},
					"slug":         schema.StringAttribute{Optional: true, Description: "The tenant's slug (default: the first free one from its name)."},
				},
			},
			"tenant_key": schema.StringAttribute{
				Computed:      true,
				Description:   "The key of the anythink tenant the org is bound to, read back from `GET /org/{slug}`; null while unbound.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"tenant_ref": schema.StringAttribute{
				Computed:      true,
				Description:   "The bound tenant's generated ref, when it has one.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"tenant_slug": schema.StringAttribute{
				Computed:      true,
				Description:   "The bound tenant's slug.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *orgResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.Conflicting(path.MatchRoot("tenant"), path.MatchRoot("new_tenant")),
	}
}

func (r *orgResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData != nil {
		r.api = clientFrom(req.ProviderData)
	}
}

// ModifyPlan refuses a replacement outright: Delete never removes an
// org (a staff act, not Terraform's), so "replace" would forget it and then fail to re-create the
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
	r.planTenant(ctx, state, plan, resp)
}

// planTenant decides what a change to `tenant` / `new_tenant` on an
// existing org means. The binding never changes by replacement — an
// org cannot be destroyed — so:
//   - a `tenant` naming the bound tenant (key, ref or slug) only records
//     the spelling; no call is made
//   - a `tenant` on an UNBOUND org binds it in place (bind-tenant)
//   - a `tenant` naming another tenant is a rebind: refused, staff's act
//   - removing `tenant` forgets the spelling; the binding stays (there is
//     no unbind)
//   - `new_tenant` is read at creation only: adding it later is refused
//     unless the org is already bound (an import adopting its config)
func (r *orgResource) planTenant(ctx context.Context, state, plan orgModel, resp *resource.ModifyPlanResponse) {
	if !plan.NewTenant.IsUnknown() && !plan.NewTenant.Equal(state.NewTenant) && !plan.NewTenant.IsNull() && !state.bound() {
		resp.Diagnostics.AddAttributeError(pathRoot("new_tenant"), "new_tenant applies only when the organization is created",
			"Organization "+state.Slug.ValueString()+" already exists and is not bound to a tenant, and Latchkey creates a tenant for an org only at its creation. "+
				"Create the tenant in anythink (or the console), then set `tenant` to it — binding an existing org is the owner's or platform staff's act.")
		return
	}
	if plan.Tenant.IsUnknown() || plan.Tenant.IsNull() || plan.Tenant.Equal(state.Tenant) {
		if plan.Tenant.IsNull() && !state.Tenant.IsNull() {
			resp.Diagnostics.AddAttributeWarning(pathRoot("tenant"), "the organization stays bound",
				"Removing `tenant` only stops Terraform asserting it: Latchkey has no unbind, and the org stays bound to "+state.TenantKey.ValueString()+".")
		}
		return
	}
	want := plan.Tenant.ValueString()
	if containsString(state.tenantNames(), want) {
		return // the bound tenant by another of its names — no call
	}
	if state.bound() {
		resp.Diagnostics.AddAttributeError(pathRoot("tenant"), "rebinding an org's tenant is a staff action",
			"Organization "+state.Slug.ValueString()+" is bound to anythink tenant "+state.TenantKey.ValueString()+describeSlug(state)+", and `tenant` now names "+want+". "+
				"A rebind changes who owns the org and where it is billed, so only platform staff makes it (POST /org/{slug}/bind-tenant as a staff person) — "+
				"Terraform never replaces an org to get there. Set `tenant` back to the bound tenant (its key, ref or slug), or have staff rebind it and then update the config.")
		return
	}
	// binding an unbound org in place: the computed names follow
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, pathRoot("tenant_key"), types.StringUnknown())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, pathRoot("tenant_ref"), types.StringUnknown())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, pathRoot("tenant_slug"), types.StringUnknown())...)
}

func describeSlug(m orgModel) string {
	if s := m.TenantSlug.ValueString(); s != "" && s != m.TenantKey.ValueString() {
		return " (\"" + s + "\")"
	}
	return ""
}

// orgCreateError explains the create refusals ADR 012 added.
func orgCreateError(err error) diag.Diagnostic {
	if api, ok := latchkey.AsAPIError(err); ok && api.Status == http.StatusBadRequest && api.TenantRequired {
		return diag.NewErrorDiagnostic("an organization must be bound to an anythink tenant",
			"Latchkey now creates every production organization on an anythink tenant (the bound-only invariant, ADR 012), and this latchkey_org names none. "+
				"Set `tenant` to a house tenant owner_email administers (its key, ref or slug), or `new_tenant = {}` to create one with the owner as its admin. "+
				"The service said: "+api.Message)
	}
	return diag.NewErrorDiagnostic("creating organization", err.Error())
}

func (r *orgResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan orgModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var nt *latchkey.NewTenant
	if !plan.NewTenant.IsNull() && !plan.NewTenant.IsUnknown() {
		var m newTenantModel
		resp.Diagnostics.Append(plan.NewTenant.As(ctx, &m, basetypes.ObjectAsOptions{})...)
		if resp.Diagnostics.HasError() {
			return
		}
		nt = &latchkey.NewTenant{DisplayName: m.DisplayName.ValueString(), Slug: m.Slug.ValueString()}
	}
	created, err := r.api.CreateOrg(ctx, plan.Slug.ValueString(), plan.DisplayName.ValueString(), plan.OwnerEmail.ValueString(), plan.Tenant.ValueString(), nt)
	if err != nil {
		resp.Diagnostics.Append(orgCreateError(err))
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
		Tenant:                plan.Tenant,
		NewTenant:             plan.NewTenant,
	}
	state.setTenant(created.Tenant)
	if created.Tenant == nil && (!plan.Tenant.IsNull() || nt != nil) {
		// an answer without the tenant view: read the binding back
		if t, err := r.api.OrgTenant(ctx, created.Slug); err == nil {
			state.setTenant(t)
		}
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
	if state.NewTenant.IsUnknown() {
		state.NewTenant = types.ObjectNull(newTenantAttrTypes)
	}
	resp.Diagnostics.Append(r.readTenant(ctx, &state)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

// readTenant reads the binding back from GET /org/{slug}. The estate's
// client reads it as platform staff; credentials without standing on
// the org (403) keep what state holds rather than fail every refresh.
// A configured `tenant` keeps its spelling while it still names the
// bound tenant; if the org was rebound (a staff act) it reads as the
// new key, so the plan shows the drift and refuses to undo it.
func (r *orgResource) readTenant(ctx context.Context, state *orgModel) diag.Diagnostics {
	var diags diag.Diagnostics
	t, err := r.api.OrgTenant(ctx, state.Slug.ValueString())
	if err != nil {
		if api, ok := latchkey.AsAPIError(err); ok && (api.Status == http.StatusForbidden || api.Status == http.StatusNotFound) {
			state.unknownTenantToNull()
			return diags
		}
		diags.AddError("reading organization's tenant", err.Error())
		return diags
	}
	state.setTenant(t)
	if !state.Tenant.IsNull() && state.bound() && !containsString(state.tenantNames(), state.Tenant.ValueString()) {
		state.Tenant = state.TenantKey
	}
	return diags
}

func (r *orgResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// the creation-time arguments RequiresReplace and ModifyPlan refuses
	// the replacement; what reaches here is `tenant` / `new_tenant`
	// (planTenant): a spelling to record, or an unbound org to bind
	var plan, state orgModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !plan.Tenant.IsNull() && !state.bound() {
		t, err := r.api.BindOrgTenant(ctx, state.Slug.ValueString(), plan.Tenant.ValueString())
		if err != nil {
			resp.Diagnostics.Append(bindError(state.Slug.ValueString(), plan.Tenant.ValueString(), err))
			return
		}
		if t == nil {
			t, _ = r.api.OrgTenant(ctx, state.Slug.ValueString())
		}
		plan.setTenant(t)
	} else {
		plan.TenantKey, plan.TenantRef, plan.TenantSlug = state.TenantKey, state.TenantRef, state.TenantSlug
	}
	plan.unknownTenantToNull()
	if plan.TerraformClientSecret.IsUnknown() {
		plan.TerraformClientSecret = state.TerraformClientSecret
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func bindError(org, tenant string, err error) diag.Diagnostic {
	if api, ok := latchkey.AsAPIError(err); ok {
		switch api.Status {
		case http.StatusForbidden:
			return diag.NewErrorDiagnostic("binding the organization to its tenant was refused",
				"Binding an existing org to an anythink tenant is a person's act — the org's owner who administers tenant "+tenant+
					", or platform staff — and the provider's credentials are neither (the service said: "+api.Message+"). "+
					"Bind it in the console (or ask staff: POST /org/"+org+"/bind-tenant), then re-run Terraform: `tenant` will match the binding.")
		case http.StatusConflict:
			return diag.NewErrorDiagnostic("rebinding an org's tenant is a staff action",
				"Organization "+org+" is already bound to another tenant (the service said: "+api.Message+"). Only platform staff rebinds an org.")
		}
	}
	return diag.NewErrorDiagnostic("binding organization to tenant", err.Error())
}

func (r *orgResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	// Terraform never deletes an org: deleting one is a platform staff
	// act outside desired state (Latchkey ADR 011, owner decision 3), and
	// the estate's client is refused it. The org stays; only state
	// forgets it.
	resp.Diagnostics.AddWarning("organization left in place",
		"Terraform never deletes a Latchkey organization — deleting one is a platform staff act (POST /platform/orgs/{slug}/delete). The org still exists upstream and importing this resource by slug adopts it (without the client secret).")
}

func (r *orgResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("terraform_client_secret"), types.StringNull())...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("new_tenant"), types.ObjectNull(newTenantAttrTypes))...)
}
