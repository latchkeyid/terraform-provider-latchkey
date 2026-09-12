package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// latchkey_api_key — one tenant API key, secret (backend, lk_live_) or
// browser/publishable (lk_pk_live_ — the Google Maps shape: origin-
// allowlisted for a web bundle, unrestricted for a native app or a
// server SDK). Keys are immutable: every attribute change replaces the
// key, which is exactly key rotation. The plaintext is captured at
// create into the sensitive `key` attribute; for a secret key that is
// the only copy that will ever exist (the service stores its hash), a
// publishable key also shows again in the tenant's key list.
type apiKeyResource struct {
	api *latchkey.Client
}

func newApiKeyResource() resource.Resource { return &apiKeyResource{} }

type apiKeyModel struct {
	ID             types.String `tfsdk:"id"`
	Tenant         types.String `tfsdk:"tenant"`
	Name           types.String `tfsdk:"name"`
	Scopes         types.List   `tfsdk:"scopes"`
	Browser        types.Bool   `tfsdk:"browser"`
	AllowedOrigins types.List   `tfsdk:"allowed_origins"`
	Key            types.String `tfsdk:"key"`
}

func (r *apiKeyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_api_key"
}

func (r *apiKeyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A tenant API key. Secret keys (lk_live_) live in the tenant's backend; " +
			"publishable keys (lk_pk_live_, `browser = true`) ship in code users can read — with " +
			"allowed_origins they verify only from those origins, without they are unrestricted " +
			"(a native app or server SDK: nothing to check an Origin against). Keys are immutable — " +
			"any change replaces the key, which is key rotation. The plaintext lands in the " +
			"sensitive `key` attribute.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, Description: "The key's display prefix.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"tenant": schema.StringAttribute{
				Required:      true,
				Description:   "The tenant the key belongs to (its slug).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				Required:      true,
				Description:   "Human name for the ledger (e.g. \"CI deploy key\").",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"scopes": schema.ListAttribute{
				ElementType:   types.StringType,
				Optional:      true,
				Description:   "Product-defined scope strings minted into the key's tokens.",
				PlanModifiers: []planmodifier.List{listplanmodifier.RequiresReplace()},
			},
			"browser": schema.BoolAttribute{
				Optional: true,
				Description: "A publishable key (lk_pk_*): embedded in code that ships to users. " +
					"With allowed_origins it verifies only from those origins; without, it is " +
					"unrestricted and identifies the tenant for quota only. Either way the allowlist " +
					"is quota protection, not a secret — never gate with a publishable key what a " +
					"secret key should.",
				PlanModifiers: []planmodifier.Bool{boolRequiresReplace()},
			},
			"allowed_origins": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Origins a browser key verifies from (https://host[:port]; " +
					"https://*.example.com covers subdomains, never the apex; http for localhost). " +
					"Only with browser = true; omit it for an unrestricted publishable key.",
				PlanModifiers: []planmodifier.List{listplanmodifier.RequiresReplace()},
			},
			"key": schema.StringAttribute{
				Computed:      true,
				Sensitive:     true,
				Description:   "The key plaintext — captured at create. For a secret key the only copy that will ever exist; a publishable key also lists in full at the service.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *apiKeyResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData != nil {
		r.api = clientFrom(req.ProviderData)
	}
}

func (r *apiKeyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan apiKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	key, prefix, err := r.api.CreateTenantKey(ctx,
		plan.Tenant.ValueString(), plan.Name.ValueString(),
		stringList(ctx, plan.Scopes),
		plan.Browser.ValueBool(),
		stringList(ctx, plan.AllowedOrigins),
	)
	if err != nil {
		resp.Diagnostics.AddError("minting API key", err.Error())
		return
	}
	plan.ID = types.StringValue(prefix)
	plan.Key = types.StringValue(key)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *apiKeyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state apiKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	keys, err := r.api.TenantKeys(ctx, state.Tenant.ValueString())
	if err != nil {
		if latchkey.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("reading API keys", err.Error())
		return
	}
	for _, k := range keys {
		if k.Prefix != state.ID.ValueString() || k.Revoked {
			continue
		}
		state.Name = types.StringValue(k.Name)
		state.Scopes = stringListValue(ctx, k.Scopes, state.Scopes)
		if k.Browser || !state.Browser.IsNull() {
			state.Browser = types.BoolValue(k.Browser)
		}
		state.AllowedOrigins = stringListValue(ctx, k.AllowedOrigins, state.AllowedOrigins)
		resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
		return
	}
	// revoked or gone — recreate on next apply
	resp.State.RemoveResource(ctx)
}

// Update never runs in practice — every attribute carries
// RequiresReplace, because a minted key cannot change and replacement IS
// rotation. If the framework ever routes here anyway, keep the computed
// identity from prior state and respect plan diagnostics.
func (r *apiKeyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state apiKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID, plan.Key = state.ID, state.Key
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *apiKeyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state apiKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.api.RevokeTenantKey(ctx, state.Tenant.ValueString(), state.ID.ValueString()); err != nil && !latchkey.IsNotFound(err) {
		resp.Diagnostics.AddError("revoking API key", err.Error())
	}
}
