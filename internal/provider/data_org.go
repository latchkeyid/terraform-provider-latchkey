package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// latchkey_org — the org's settings snapshot. Secrets never appear;
// configured-ness booleans say what's wired.
type orgDataSource struct {
	api *latchkey.Client
}

func newOrgDataSource() datasource.DataSource { return &orgDataSource{} }

type orgDataModel struct {
	Slug            types.String `tfsdk:"slug"`
	DisplayName     types.String `tfsdk:"display_name"`
	MailConfigured  types.Bool   `tfsdk:"mail_configured"`
	PhoneConfigured types.Bool   `tfsdk:"phone_configured"`
	GithubSignin    types.String `tfsdk:"github_signin"`
	GithubAppID     types.String `tfsdk:"github_app_id"`
}

func (d *orgDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org"
}

func (d *orgDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "The provider's organization: identity and what's configured. Secrets never appear.",
		Attributes: map[string]schema.Attribute{
			"slug":             schema.StringAttribute{Computed: true},
			"display_name":     schema.StringAttribute{Computed: true},
			"mail_configured":  schema.BoolAttribute{Computed: true, Description: "Whether a SendGrid sender is wired."},
			"phone_configured": schema.BoolAttribute{Computed: true, Description: "Whether Twilio Verify is wired."},
			"github_signin":    schema.StringAttribute{Computed: true, Description: "Continue-with-GitHub mode: \"\" (off), \"platform\" or \"custom\"."},
			"github_app_id":    schema.StringAttribute{Computed: true, Description: "The org's GitHub App id held for the proxy; \"\" when none."},
		},
	}
}

func (d *orgDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, _ *datasource.ConfigureResponse) {
	if req.ProviderData != nil {
		d.api = clientFrom(req.ProviderData)
	}
}

func (d *orgDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	o, err := d.api.GetOrg(ctx)
	if err != nil {
		resp.Diagnostics.AddError("reading org", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, orgDataModel{
		Slug:            types.StringValue(o.Slug),
		DisplayName:     types.StringValue(o.DisplayName),
		MailConfigured:  types.BoolValue(o.MailConfigured),
		PhoneConfigured: types.BoolValue(o.PhoneConfigured),
		GithubSignin:    types.StringValue(o.SocialGithubMode),
		GithubAppID:     types.StringValue(o.GithubAppID),
	})...)
}
