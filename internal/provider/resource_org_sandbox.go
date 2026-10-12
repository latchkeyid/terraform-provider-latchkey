package provider

import (
	"context"
	"net/http"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// latchkey_org_sandbox — one of the org's NAMED environment sandboxes
// (Latchkey ADR 012 slice 2): a sibling org slugged {slug}-{name},
// owned by this org's owner, pointing back to it. Everything inside
// (clients, tenants, grants, keys) is isolated by construction;
// production standing implies sandbox standing, never the reverse, and
// every key minted inside is a test key. `name` defaults to "sandbox" —
// the one sandbox an org had before names, so state written by earlier
// versions of this resource reads unchanged.
//
// Manage the sandbox's CONTENTS with a second provider alias pointed at
// the sandbox slug (`org = latchkey_org_sandbox.env.slug`) — the
// production org's service client is authorized there through the same
// implication (latchkey #78).
//
// A sandbox is disposable (unlike a production org, ADR 011 decision 3):
// destroying this resource DELETES the sandbox org, its tenants, clients
// and members with it — the production org's service client may.
type orgSandboxResource struct {
	api *latchkey.Client
}

func newOrgSandboxResource() resource.Resource { return &orgSandboxResource{} }

// sandboxDefaultName is the name of the sandbox from before names
// ({slug}-sandbox), and the default.
const sandboxDefaultName = "sandbox"

// sandboxNameRe is the live rule: 1–20 of a–z, 0–9 and hyphens, not at
// the ends. The service lowercases; config must already be lowercase.
var sandboxNameRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,18}[a-z0-9])?$`)

type orgSandboxModel struct {
	ID        types.String `tfsdk:"id"`
	Name      types.String `tfsdk:"name"`
	Slug      types.String `tfsdk:"slug"`
	Key       types.String `tfsdk:"key"`
	SandboxOf types.String `tfsdk:"sandbox_of"`
}

// orgSandboxModelV0 is the schema before names: one sandbox per org.
type orgSandboxModelV0 struct {
	ID        types.String `tfsdk:"id"`
	Slug      types.String `tfsdk:"slug"`
	SandboxOf types.String `tfsdk:"sandbox_of"`
}

func (r *orgSandboxResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_sandbox"
}

func orgSandboxSchemaV0() schema.Schema {
	return schema.Schema{
		Attributes: map[string]schema.Attribute{
			"id":         schema.StringAttribute{Computed: true},
			"slug":       schema.StringAttribute{Computed: true},
			"sandbox_of": schema.StringAttribute{Computed: true},
		},
	}
}

func (r *orgSandboxResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Version: 1,
		Description: "One of the org's named environment sandboxes — a sibling org slugged {org}-{name}, isolated by construction, that production standing implies membership of. " +
			"`name` defaults to `sandbox` ({org}-sandbox, the one sandbox an org had before names). Point a second provider alias at `slug` to manage its contents. " +
			"A sandbox is disposable: destroy DELETES the sandbox org and everything in it.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "The sandbox org's slug.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Default:       stringdefault.StaticString(sandboxDefaultName),
				Description:   "The sandbox's name, unique per org: 1–20 lowercase characters (a–z, 0–9, hyphens not at the ends). Default `sandbox`. Changing it replaces the sandbox (the old one is deleted).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.RegexMatches(sandboxNameRe, "must be 1–20 lowercase characters: a–z, 0–9, hyphens not at the ends")},
			},
			"slug": schema.StringAttribute{
				Computed:      true,
				Description:   "The sandbox org's slug ({org}-{name}) — the `org` for a provider alias that manages its contents.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"key": schema.StringAttribute{
				Computed:      true,
				Description:   "The sandbox org's key (its generated ref, for a ref org).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"sandbox_of": schema.StringAttribute{
				Computed:      true,
				Description:   "The production org this is a sandbox of (the provider's org).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// UpgradeState: state from before names (version 0) is the sandbox
// named "sandbox" — the one {slug}-sandbox — so it reads with no diff.
func (r *orgSandboxResource) UpgradeState(context.Context) map[int64]resource.StateUpgrader {
	v0 := orgSandboxSchemaV0()
	return map[int64]resource.StateUpgrader{
		0: {
			PriorSchema: &v0,
			StateUpgrader: func(ctx context.Context, req resource.UpgradeStateRequest, resp *resource.UpgradeStateResponse) {
				var old orgSandboxModelV0
				resp.Diagnostics.Append(req.State.Get(ctx, &old)...)
				if resp.Diagnostics.HasError() {
					return
				}
				resp.Diagnostics.Append(resp.State.Set(ctx, orgSandboxModel{
					ID:        old.ID,
					Name:      types.StringValue(sandboxDefaultName),
					Slug:      old.Slug,
					Key:       types.StringNull(),
					SandboxOf: old.SandboxOf,
				})...)
			},
		},
	}
}

func (r *orgSandboxResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData != nil {
		r.api = clientFrom(req.ProviderData)
	}
}

// routeMissing is a server from before named sandboxes answering a
// route it does not have.
func routeMissing(err error) bool {
	api, ok := latchkey.AsAPIError(err)
	return ok && (api.Status == http.StatusNotFound || api.Status == http.StatusMethodNotAllowed)
}

func (r *orgSandboxResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan orgSandboxModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := plan.Name.ValueString()
	state := orgSandboxModel{Name: plan.Name, SandboxOf: types.StringValue(r.api.Org), Key: types.StringNull()}
	sb, err := r.api.CreateNamedSandbox(ctx, name)
	switch {
	case err == nil:
		state.Slug = types.StringValue(sb.Slug)
		state.Key = stringOrNull(sb.Key, types.StringNull())
	case name == sandboxDefaultName && routeMissing(err):
		// a server from before named sandboxes: its one sandbox
		slug, err := r.api.CreateSandbox(ctx)
		if err != nil {
			resp.Diagnostics.AddError("creating org sandbox", err.Error())
			return
		}
		state.Slug = types.StringValue(slug)
	default:
		resp.Diagnostics.AddError("creating org sandbox", err.Error())
		return
	}
	state.ID = state.Slug
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *orgSandboxResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state orgSandboxModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	if state.Name.IsNull() || name == "" {
		name = sandboxDefaultName
		state.Name = types.StringValue(name)
	}
	items, err := r.api.Sandboxes(ctx)
	if err != nil && name == sandboxDefaultName && routeMissing(err) {
		org, oerr := r.api.GetOrg(ctx)
		if oerr != nil {
			resp.Diagnostics.AddError("reading org", oerr.Error())
			return
		}
		items, err = nil, nil
		if org.Sandbox != "" {
			items = []latchkey.Sandbox{{Name: sandboxDefaultName, Slug: org.Sandbox}}
		}
	}
	if err != nil {
		resp.Diagnostics.AddError("reading org sandboxes", err.Error())
		return
	}
	var found *latchkey.Sandbox
	for i := range items {
		if items[i].Name == name {
			found = &items[i]
		}
	}
	if found == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	state.ID = types.StringValue(found.Slug)
	state.Slug = types.StringValue(found.Slug)
	state.Key = stringOrNull(found.Key, types.StringNull())
	state.SandboxOf = types.StringValue(r.api.Org)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *orgSandboxResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// name RequiresReplace and the rest is computed — Update never runs,
	// but the interface wants it
	var plan orgSandboxModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *orgSandboxResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state orgSandboxModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	if name == "" {
		name = sandboxDefaultName
	}
	err := r.api.DeleteSandbox(ctx, name, state.Slug.ValueString())
	if err == nil {
		return
	}
	if api, ok := latchkey.AsAPIError(err); ok && api.Status == http.StatusNotFound {
		// already gone — or a server from before named sandboxes, which
		// has no sandbox delete: say so rather than pretend
		if _, lerr := r.api.Sandboxes(ctx); lerr != nil && routeMissing(lerr) {
			resp.Diagnostics.AddWarning("org sandbox left in place",
				"This Latchkey server has no sandbox delete (named sandboxes, ADR 012 slice 2, are not deployed there). The sandbox org "+state.Slug.ValueString()+" still exists; only state forgets it.")
		}
		return
	}
	resp.Diagnostics.AddError("deleting org sandbox", err.Error())
}

func (r *orgSandboxResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// import by the sandbox's NAME (staging, preview-42, sandbox)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("name"), req.ID)...)
}
