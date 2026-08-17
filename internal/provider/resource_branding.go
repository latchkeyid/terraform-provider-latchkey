package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// latchkey_branding — the org's hosted-page dress. A singleton: its id is
// the org slug, and destroy returns the pages to the plain default card.
// Hosted image uploads (logo/background files) are console territory —
// this manages the declarative half: URL, colours, tagline, backdrop
// style.
type brandingResource struct {
	api *latchkey.Client
}

func newBrandingResource() resource.Resource { return &brandingResource{} }

type brandingModel struct {
	ID         types.String `tfsdk:"id"`
	LogoURL    types.String `tfsdk:"logo_url"`
	Accent     types.String `tfsdk:"accent"`
	BgColor    types.String `tfsdk:"bg_color"`
	Tagline    types.String `tfsdk:"tagline"`
	BgFit      types.String `tfsdk:"bg_fit"`
	BgPosition types.String `tfsdk:"bg_position"`
	BgScrim    types.String `tfsdk:"bg_scrim"`
}

func (r *brandingResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_branding"
}

func (r *brandingResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "The org's hosted sign-in page dress. Singleton — one per org; destroy restores the plain default card. Hosted image uploads stay in the console; this manages the declarative half.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, Description: "The org slug."},
			"logo_url": schema.StringAttribute{
				Optional:    true,
				Description: "https URL of a logo the org hosts itself.",
			},
			"accent": schema.StringAttribute{
				Optional:    true,
				Description: "Hex accent colour for the primary button (e.g. #1a73e8). The label colour is derived from luminance.",
			},
			"bg_color": schema.StringAttribute{
				Optional:    true,
				Description: "Hex page background colour. When set, the brand wins over the visitor's light/dark theme.",
			},
			"tagline": schema.StringAttribute{
				Optional:    true,
				Description: "One line under the sign-in heading. 140 characters, no markup.",
			},
			"bg_fit": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString("cover"),
				Description: "How an uploaded background image is drawn: cover, contain or tile.",
				Validators:  []validator.String{stringvalidator.OneOf("cover", "contain", "tile")},
			},
			"bg_position": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString("center"),
				Description: "Background anchor: center, top, bottom, left or right.",
				Validators:  []validator.String{stringvalidator.OneOf("center", "top", "bottom", "left", "right")},
			},
			"bg_scrim": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString("dark"),
				Description: "Legibility scrim over the background image: dark, soft or none.",
				Validators:  []validator.String{stringvalidator.OneOf("dark", "soft", "none")},
			},
		},
	}
}

func (r *brandingResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData != nil {
		r.api = clientFrom(req.ProviderData)
	}
}

func (r *brandingResource) apply(ctx context.Context, plan brandingModel) error {
	if err := r.api.SetBranding(ctx, latchkey.Branding{
		LogoURL: plan.LogoURL.ValueString(),
		Accent:  plan.Accent.ValueString(),
		Bg:      plan.BgColor.ValueString(),
		Tagline: plan.Tagline.ValueString(),
	}); err != nil {
		return err
	}
	return r.api.SetBackgroundStyle(ctx, plan.BgFit.ValueString(), plan.BgPosition.ValueString(), plan.BgScrim.ValueString())
}

func (r *brandingResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan brandingModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.apply(ctx, plan); err != nil {
		resp.Diagnostics.AddError("setting branding", err.Error())
		return
	}
	plan.ID = types.StringValue(r.api.Org)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *brandingResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state brandingModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	o, err := r.api.GetOrg(ctx)
	if err != nil {
		resp.Diagnostics.AddError("reading org branding", err.Error())
		return
	}
	state.ID = types.StringValue(o.Slug)
	state.LogoURL = stringOrNull(o.BrandLogoURL, state.LogoURL)
	state.Accent = stringOrNull(o.BrandAccent, state.Accent)
	state.BgColor = stringOrNull(o.BrandBg, state.BgColor)
	state.Tagline = stringOrNull(o.BrandTagline, state.Tagline)
	// unset style reads as the defaults the pages render
	state.BgFit = defaulted(o.BrandBgFit, "cover")
	state.BgPosition = defaulted(o.BrandBgPosition, "center")
	state.BgScrim = defaulted(o.BrandBgScrim, "dark")
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func defaulted(v, def string) types.String {
	if v == "" {
		return types.StringValue(def)
	}
	return types.StringValue(v)
}

func (r *brandingResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan brandingModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.apply(ctx, plan); err != nil {
		resp.Diagnostics.AddError("updating branding", err.Error())
		return
	}
	plan.ID = types.StringValue(r.api.Org)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *brandingResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if err := r.api.ClearBranding(ctx); err != nil && !latchkey.IsNotFound(err) {
		resp.Diagnostics.AddError("clearing branding", err.Error())
	}
}
