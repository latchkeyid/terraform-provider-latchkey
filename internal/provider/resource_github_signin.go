package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// latchkey_github_signin — Continue with GitHub on the org's hosted
// sign-in pages. A singleton: its id is the org slug, destroy takes the
// button away. Mode "platform" borrows Latchkey's own GitHub OAuth app;
// "custom" is the org's own — required on a custom auth domain (a GitHub
// OAuth app has ONE callback URL), and what makes people's tokens GitHub
// App user tokens when the client is a GitHub App's, which the proxy's
// as-user calls to /user/installations need. The secret is write-only:
// never read back, kept in state as last applied.
type githubSigninResource struct {
	api *latchkey.Client
}

func newGithubSigninResource() resource.Resource { return &githubSigninResource{} }

type githubSigninModel struct {
	ID           types.String `tfsdk:"id"`
	Mode         types.String `tfsdk:"mode"`
	ClientID     types.String `tfsdk:"client_id"`
	ClientSecret types.String `tfsdk:"client_secret"`
}

func (r *githubSigninResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_github_signin"
}

func (r *githubSigninResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Continue with GitHub on the org's hosted sign-in pages. Singleton — one per org; destroy removes the button. " +
			"mode \"platform\" borrows Latchkey's GitHub OAuth app; \"custom\" is the org's own client id + secret — required on a custom auth domain, " +
			"and use the org's GitHub App's client so sign-ins yield GitHub App user tokens (what the proxy's as-user calls need). " +
			"The callback URL to register on GitHub is {issuer}/oauth/social/callback.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, Description: "The org slug."},
			"mode": schema.StringAttribute{
				Required:    true,
				Description: "\"platform\" or \"custom\".",
				Validators:  []validator.String{stringvalidator.OneOf("platform", "custom")},
			},
			"client_id": schema.StringAttribute{
				Optional:    true,
				Description: "The OAuth client id (custom mode). A GitHub App's is Iv1.…; an OAuth App's is 20 hex characters.",
			},
			"client_secret": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "The client secret (custom mode). Write-only — never read back; a changed value rotates it.",
			},
		},
	}
}

func (r *githubSigninResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	r.api = req.ProviderData.(*latchkey.Client)
}

func (r *githubSigninResource) apply(ctx context.Context, m githubSigninModel) error {
	return r.api.SetGithubSignin(ctx, m.Mode.ValueString(), m.ClientID.ValueString(), m.ClientSecret.ValueString())
}

func (r *githubSigninResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan githubSigninModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.apply(ctx, plan); err != nil {
		resp.Diagnostics.AddError("setting GitHub sign-in", err.Error())
		return
	}
	plan.ID = types.StringValue(r.api.Org)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *githubSigninResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state githubSigninModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	o, err := r.api.GetOrg(ctx)
	if err != nil {
		resp.Diagnostics.AddError("reading org GitHub sign-in", err.Error())
		return
	}
	if o.SocialGithubMode == "" {
		// switched off in the console — recreate on the next apply
		resp.State.RemoveResource(ctx)
		return
	}
	state.ID = types.StringValue(o.Slug)
	state.Mode = types.StringValue(o.SocialGithubMode)
	state.ClientID = stringOrNull(o.SocialGithubClientID, state.ClientID)
	// the secret is write-only; state keeps what was last applied
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *githubSigninResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan githubSigninModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.apply(ctx, plan); err != nil {
		resp.Diagnostics.AddError("updating GitHub sign-in", err.Error())
		return
	}
	plan.ID = types.StringValue(r.api.Org)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *githubSigninResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if err := r.api.ClearGithubSignin(ctx); err != nil && !latchkey.IsNotFound(err) {
		resp.Diagnostics.AddError("clearing GitHub sign-in", err.Error())
	}
}
