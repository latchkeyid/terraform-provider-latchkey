package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// latchkey_federation_target_grant — one product's grant on a federation
// target: what it may have subject tokens minted for. The grant endpoint
// replaces the product's grant, so purposes, kinds and environments
// update in place. There is no read of one grant: the target's own read
// lists them, and a product missing from that list is a grant revoked.
type federationTargetGrantResource struct {
	api *latchkey.Client
}

func newFederationTargetGrantResource() resource.Resource { return &federationTargetGrantResource{} }

var (
	_ resource.ResourceWithModifyPlan  = (*federationTargetGrantResource)(nil)
	_ resource.ResourceWithImportState = (*federationTargetGrantResource)(nil)
)

type federationTargetGrantModel struct {
	ID           types.String `tfsdk:"id"`
	Org          types.String `tfsdk:"org"`
	Tenant       types.String `tfsdk:"tenant"`
	Target       types.String `tfsdk:"target"`
	Project      types.String `tfsdk:"project"`
	Product      types.String `tfsdk:"product"`
	Purposes     types.Set    `tfsdk:"purposes"`
	Kinds        types.Set    `tfsdk:"kinds"`
	Environments types.Set    `tfsdk:"environments"`
}

func (r *federationTargetGrantResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_federation_target_grant"
}

func (r *federationTargetGrantResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		Description: "One product's grant on a federation target: the purposes it may have subject tokens minted for, " +
			"the job kinds whose sessions may mint, and the environments. A product with no grant is minted nothing.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, Description: "org/tenant/target/product, with /project for a project's target.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"org": federationOrgAttribute(),
			"tenant": schema.StringAttribute{
				Required:      true,
				Description:   "The tenant the target belongs to.",
				PlanModifiers: replace,
			},
			"target": schema.StringAttribute{
				Required:      true,
				Description:   "The target's name (latchkey_federation_target.x.name).",
				Validators:    []validator.String{stringvalidator.RegexMatches(federationNameRe, "must be lower-case letters, digits and dashes (at most 63)")},
				PlanModifiers: replace,
			},
			"project": schema.StringAttribute{
				Optional:      true,
				Description:   "The target's project, for a project's target.",
				Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
				PlanModifiers: replace,
			},
			"product": schema.StringAttribute{
				Required:      true,
				Description:   "The product granted (its lower-case slug, e.g. `runsheet`).",
				Validators:    []validator.String{stringvalidator.RegexMatches(federationGrantRe, "must be a lower-case product slug")},
				PlanModifiers: replace,
			},
			"purposes": schema.SetAttribute{
				ElementType: types.StringType,
				Required:    true,
				Description: "The purposes the product may mint for (`deploy`, `configure`, `observe`, …), lower-case. At least one.",
				Validators: []validator.Set{setvalidator.SizeAtLeast(1), setvalidator.ValueStringsAre(
					stringvalidator.RegexMatches(federationPurposeRe, "must be a lower-case purpose name"))},
			},
			"kinds": schema.SetAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "The job kinds whose sessions may mint; `*` is every kind. Omitted, only the product's own workload mints.",
				Validators: []validator.Set{setvalidator.SizeAtLeast(1), setvalidator.ValueStringsAre(
					stringvalidator.RegexMatches(federationKindRe, "must be a lower-case kind name, or *"))},
			},
			"environments": schema.SetAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "The environments a mint may name (`production`). Omitted, any.",
				Validators: []validator.Set{setvalidator.SizeAtLeast(1), setvalidator.ValueStringsAre(
					stringvalidator.RegexMatches(federationGrantRe, "must be a lower-case environment name"))},
			},
		},
	}
}

func (r *federationTargetGrantResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData != nil {
		r.api = clientFrom(req.ProviderData)
	}
}

// ModifyPlan names the provider's org where the configuration names none.
func (r *federationTargetGrantResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || r.api == nil {
		return
	}
	var org types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, pathRoot("org"), &org)...)
	if resp.Diagnostics.HasError() || !org.IsUnknown() {
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, pathRoot("org"), r.api.Org)...)
}

func (r *federationTargetGrantResource) put(ctx context.Context, plan *federationTargetGrantModel) error {
	org, tenant := plan.Org.ValueString(), plan.Tenant.ValueString()
	t, err := r.api.SetFederationTargetGrant(ctx, org, tenant, plan.Target.ValueString(), plan.Project.ValueString(), plan.Product.ValueString(),
		stringSet(ctx, plan.Purposes), stringSet(ctx, plan.Kinds), stringSet(ctx, plan.Environments))
	if err != nil {
		if latchkey.IsNotFound(err) {
			return fmt.Errorf("%s — no federation target %q on tenant %q of organization %q (or the organization does not hold the federation feature)",
				err.Error(), plan.Target.ValueString(), tenant, org)
		}
		return err
	}
	g := t.GrantFor(plan.Product.ValueString())
	if g == nil {
		return fmt.Errorf("the target's answer lists no grant for product %q", plan.Product.ValueString())
	}
	r.fill(ctx, plan, g)
	return nil
}

func (r *federationTargetGrantResource) fill(ctx context.Context, m *federationTargetGrantModel, g *latchkey.FederationTargetGrant) {
	id := m.Org.ValueString() + "/" + m.Tenant.ValueString() + "/" + m.Target.ValueString() + "/" + m.Product.ValueString()
	if p := m.Project.ValueString(); p != "" {
		id += "/" + p
	}
	m.ID = types.StringValue(id)
	m.Purposes, _ = types.SetValueFrom(ctx, types.StringType, g.Purposes)
	m.Kinds = stringSetValue(ctx, g.Kinds, m.Kinds)
	m.Environments = stringSetValue(ctx, g.Environments, m.Environments)
}

func (r *federationTargetGrantResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan federationTargetGrantModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.put(ctx, &plan); err != nil {
		resp.Diagnostics.AddError("granting federation target", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *federationTargetGrantResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state federationTargetGrantModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	t, err := r.api.GetFederationTarget(ctx, state.Org.ValueString(), state.Tenant.ValueString(), state.Target.ValueString(), state.Project.ValueString())
	if err != nil {
		// the target is gone, and its grants with it
		if latchkey.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("reading federation target", err.Error())
		return
	}
	g := t.GrantFor(state.Product.ValueString())
	if g == nil {
		// revoked — grant again on the next apply
		resp.State.RemoveResource(ctx)
		return
	}
	r.fill(ctx, &state, g)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *federationTargetGrantResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan federationTargetGrantModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.put(ctx, &plan); err != nil {
		resp.Diagnostics.AddError("updating federation target grant", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *federationTargetGrantResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state federationTargetGrantModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// a deleted target (404) took its grants with it
	err := r.api.RevokeFederationTargetGrant(ctx, state.Org.ValueString(), state.Tenant.ValueString(), state.Target.ValueString(),
		state.Project.ValueString(), state.Product.ValueString())
	if err != nil && !latchkey.IsNotFound(err) {
		resp.Diagnostics.AddError("revoking federation target grant", err.Error())
	}
}

// ImportState reads org/tenant/target/product, or
// org/tenant/target/product/project for a project's target.
func (r *federationTargetGrantResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// a project's ref may itself hold slashes: it is the rest
	parts := strings.SplitN(req.ID, "/", 5)
	if len(parts) < 4 || slicesHasEmpty(parts) {
		resp.Diagnostics.AddError("unexpected import id", fmt.Sprintf("Import a federation target grant by org/tenant/target/product (or org/tenant/target/product/project for a project's target); got %q.", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("org"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("tenant"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("target"), parts[2])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("product"), parts[3])...)
	if len(parts) == 5 {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("project"), parts[4])...)
	}
}
