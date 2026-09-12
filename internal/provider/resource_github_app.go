package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// latchkey_github_app — the org's GitHub App, held by Latchkey for the
// GitHub proxy: the product calls GitHub as the App (as-app) or as one of
// its installations (as-installation) and Latchkey mints the JWT and
// installation tokens from the key it keeps, so the product's runtime
// never holds the key. A singleton keyed by the org slug; destroy makes
// those calls refuse. The key is write-only: never read back, kept in
// state as last applied; the service parses it at set time.
type githubAppResource struct {
	api *latchkey.Client
}

func newGithubAppResource() resource.Resource { return &githubAppResource{} }

type githubAppModel struct {
	ID         types.String `tfsdk:"id"`
	AppID      types.String `tfsdk:"app_id"`
	PrivateKey types.String `tfsdk:"private_key"`
}

func (r *githubAppResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_github_app"
}

func (r *githubAppResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "The org's GitHub App, kept by Latchkey for the GitHub proxy (/org/{slug}/github/as-app and as-installation). " +
			"Singleton — one per org; destroy makes those calls refuse. The private key is write-only: parsed when set, never read back.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, Description: "The org slug."},
			"app_id": schema.StringAttribute{
				Required:    true,
				Description: "The App's numeric id (GitHub → Settings → Developer settings → GitHub Apps → About).",
			},
			"private_key": schema.StringAttribute{
				Required:    true,
				Sensitive:   true,
				Description: "The App's private key — the .pem GitHub generated (PKCS#1 \"RSA PRIVATE KEY\", or PKCS#8). Write-only; a changed value rotates it.",
			},
		},
	}
}

func (r *githubAppResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	r.api = req.ProviderData.(*latchkey.Client)
}

func (r *githubAppResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan githubAppModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.api.SetGithubApp(ctx, plan.AppID.ValueString(), plan.PrivateKey.ValueString()); err != nil {
		resp.Diagnostics.AddError("setting GitHub App", err.Error())
		return
	}
	plan.ID = types.StringValue(r.api.Org)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *githubAppResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state githubAppModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	o, err := r.api.GetOrg(ctx)
	if err != nil {
		resp.Diagnostics.AddError("reading org GitHub App", err.Error())
		return
	}
	if !o.GithubAppConfigured {
		// removed in the console — recreate on the next apply
		resp.State.RemoveResource(ctx)
		return
	}
	state.ID = types.StringValue(o.Slug)
	state.AppID = types.StringValue(o.GithubAppID)
	// the key is write-only; state keeps what was last applied
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *githubAppResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan githubAppModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.api.SetGithubApp(ctx, plan.AppID.ValueString(), plan.PrivateKey.ValueString()); err != nil {
		resp.Diagnostics.AddError("updating GitHub App", err.Error())
		return
	}
	plan.ID = types.StringValue(r.api.Org)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *githubAppResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if err := r.api.ClearGithubApp(ctx); err != nil && !latchkey.IsNotFound(err) {
		resp.Diagnostics.AddError("clearing GitHub App", err.Error())
	}
}
