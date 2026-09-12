package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// latchkey_org_sandbox — the org's environment sandbox: a sibling org
// named {slug}-sandbox, owned by this org's owner, pointing back through
// sandbox_of. Everything inside (clients, tenants, grants, keys) is
// isolated by construction; production standing implies sandbox
// standing, never the reverse, and every key minted inside is a test
// key. One per org, so the resource takes no arguments.
//
// Manage the sandbox's CONTENTS with a second provider alias pointed at
// the sandbox slug (`org = latchkey_org_sandbox.env.slug`) — the
// production org's service client is authorized there through the same
// implication (latchkey #78).
//
// Latchkey has no org delete: destroying this resource forgets the
// sandbox from state and leaves it in place upstream.
type orgSandboxResource struct {
	api *latchkey.Client
}

func newOrgSandboxResource() resource.Resource { return &orgSandboxResource{} }

type orgSandboxModel struct {
	ID        types.String `tfsdk:"id"`
	Slug      types.String `tfsdk:"slug"`
	SandboxOf types.String `tfsdk:"sandbox_of"`
}

func (r *orgSandboxResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_sandbox"
}

func (r *orgSandboxResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "The org's environment sandbox — a sibling org named {slug}-sandbox, isolated by construction, that production standing implies membership of. One per org; takes no arguments. Point a second provider alias at `slug` to manage its contents. Destroy only forgets it from state (Latchkey has no org delete).",
		Attributes: map[string]schema.Attribute{
			"id":   schema.StringAttribute{Computed: true, Description: "The sandbox org's slug."},
			"slug": schema.StringAttribute{Computed: true, Description: "The sandbox org's slug ({org}-sandbox) — the `org` for a provider alias that manages its contents."},
			"sandbox_of": schema.StringAttribute{
				Computed:    true,
				Description: "The production org this is the sandbox of (the provider's org).",
			},
		},
	}
}

func (r *orgSandboxResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData != nil {
		r.api = clientFrom(req.ProviderData)
	}
}

func (r *orgSandboxResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	slug, err := r.api.CreateSandbox(ctx)
	if err != nil {
		resp.Diagnostics.AddError("creating org sandbox", err.Error())
		return
	}
	state := orgSandboxModel{
		ID:        types.StringValue(slug),
		Slug:      types.StringValue(slug),
		SandboxOf: types.StringValue(r.api.Org),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *orgSandboxResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state orgSandboxModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	org, err := r.api.GetOrg(ctx)
	if err != nil {
		resp.Diagnostics.AddError("reading org", err.Error())
		return
	}
	if org.Sandbox == "" {
		resp.State.RemoveResource(ctx)
		return
	}
	state.ID = types.StringValue(org.Sandbox)
	state.Slug = types.StringValue(org.Sandbox)
	state.SandboxOf = types.StringValue(r.api.Org)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *orgSandboxResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// no arguments — Update never runs, but the interface wants it
	var plan orgSandboxModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *orgSandboxResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	// Latchkey has no org delete: the sandbox stays; only state forgets it.
	resp.Diagnostics.AddWarning("org sandbox left in place",
		"Latchkey does not delete organizations; the sandbox org still exists upstream and re-adding this resource adopts it.")
}
