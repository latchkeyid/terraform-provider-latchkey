package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// latchkey_client — an OIDC client of the org. Deletion disables (client
// ids are never reused); public/confidential and the secret are fixed at
// registration, so changing them replaces the client — a new client id,
// flagged by Terraform's plan as such.
type clientResource struct {
	api *latchkey.Client
}

func newClientResource() resource.Resource { return &clientResource{} }

var _ resource.ResourceWithModifyPlan = (*clientResource)(nil)

type clientModel struct {
	ID                  types.String `tfsdk:"id"`
	Name                types.String `tfsdk:"name"`
	Public              types.Bool   `tfsdk:"public"`
	Secret              types.String `tfsdk:"secret"`
	RedirectURIs        types.List   `tfsdk:"redirect_uris"`
	PhoneEnabled        types.Bool   `tfsdk:"phone_enabled"`
	CaptchaRequired     types.Bool   `tfsdk:"captcha_required"`
	AttestationRequired types.Bool   `tfsdk:"attestation_required"`
	OtpDailyCeiling     types.Int64  `tfsdk:"otp_daily_ceiling"`
	Exchange            types.Object `tfsdk:"exchange"`
	ReviewLogin         types.Object `tfsdk:"review_login"`
}

// the review_login block: who signs in with the fixed code
type reviewLoginModel struct {
	Email  types.String `tfsdk:"email"`
	Phone  types.String `tfsdk:"phone"`
	Domain types.String `tfsdk:"domain"`
	Code   types.String `tfsdk:"code"`
}

var reviewLoginAttrTypes = map[string]attr.Type{
	"email":  types.StringType,
	"phone":  types.StringType,
	"domain": types.StringType,
	"code":   types.StringType,
}

// exchangeModel is the `exchange` block: the client as an RFC 8693 actor.
type exchangeModel struct {
	Audiences types.List  `tfsdk:"audiences"`
	Claims    types.List  `tfsdk:"claims"`
	Subject   types.Bool  `tfsdk:"subject"`
	TTL       types.Int64 `tfsdk:"ttl"`
}

var exchangeAttrTypes = map[string]attr.Type{
	"audiences": types.ListType{ElemType: types.StringType},
	"claims":    types.ListType{ElemType: types.StringType},
	"subject":   types.BoolType,
	"ttl":       types.Int64Type,
}

func (r *clientResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_client"
}

func (r *clientResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "An OIDC client of the organization. Destroy disables the client — client ids are never reused.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Client id (uuid).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Display name — the hosted pages say \"Sign in to {name}\" for untagged clients.",
			},
			"public": schema.BoolAttribute{
				Optional: true, Computed: true,
				Default:     booldefault.StaticBool(true),
				Description: "Public (SPA/native, PKCE-only) vs confidential. Fixed at registration; changing it replaces the client.",
				PlanModifiers: []planmodifier.Bool{
					boolRequiresReplace(),
				},
			},
			"secret": schema.StringAttribute{
				Optional: true, Sensitive: true,
				Description: "Client secret for confidential clients. Hashed client-side (sha256) before it leaves Terraform; changing it replaces the client.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"redirect_uris": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Registered redirect URIs. Updatable in place.",
			},
			"phone_enabled": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Allow the phone (SMS OTP) grant. Requires the org's Twilio Verify config.",
			},
			"captcha_required": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Require a Turnstile token on sign-in. Requires the org's Turnstile keys; fails closed.",
			},
			"attestation_required": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Require device attestation (Play Integrity / App Attest). Fails closed.",
			},
			"otp_daily_ceiling": schema.Int64Attribute{
				Optional: true, Computed: true, Default: int64default.StaticInt64(0),
				Description: "Daily SMS budget for this client; 0 uses the platform default.",
			},
			"exchange": schema.SingleNestedAttribute{
				Optional: true,
				Description: "Token exchange (RFC 8693) with this client as the actor: what it may mint at /oauth/token " +
					"from a live Latchkey subject token. Confidential clients only. Omit the block to turn the grant off.",
				Attributes: map[string]schema.Attribute{
					"audiences": schema.ListAttribute{
						ElementType: types.StringType,
						Required:    true,
						Description: "Audiences the actor may mint for. Never Latchkey's own (`latchkey`, `latchkey-session`) or a client id.",
					},
					"claims": schema.ListAttribute{
						ElementType: types.StringType,
						Optional:    true,
						Description: "Top-level claim names the actor may supply (via the `claims` form field). Reserved names are refused.",
					},
					"subject": schema.BoolAttribute{
						Optional: true, Computed: true, Default: booldefault.StaticBool(false),
						Description: "Whether the actor may replace `sub` — the shape a cloud's trust policy matches on.",
					},
					"ttl": schema.Int64Attribute{
						Optional: true, Computed: true, Default: int64default.StaticInt64(0),
						Description: "Ceiling on a minted token's lifetime, seconds (60–86400); 0 = 1h. `expires_in` may shorten it per token, never lengthen.",
					},
				},
			},
			"review_login": schema.SingleNestedAttribute{
				Optional: true,
				Description: "The review / test-identity sign-in: the named email and/or phone, and every address under `domain`, " +
					"sign in with the fixed `code` and receive no email or SMS (the mailbox or number need not exist). " +
					"For app-store reviewers (the store's App Review sign-in fields) and for the test users of an acceptance suite. " +
					"Public clients only. Omit the block to turn it off. At least one of email, phone, domain is required.",
				Attributes: map[string]schema.Attribute{
					"email": schema.StringAttribute{
						Optional: true, Computed: true, Default: stringdefault.StaticString(""),
						Description: "One address that signs in with the code.",
					},
					"phone": schema.StringAttribute{
						Optional: true, Computed: true, Default: stringdefault.StaticString(""),
						Description: "One E.164 number that signs in with the code.",
					},
					"domain": schema.StringAttribute{
						Optional: true, Computed: true, Default: stringdefault.StaticString(""),
						Description: "A bare domain you control (e.g. `review.example.com`): every address under it signs in with the code. Exact match — subdomains do not.",
					},
					"code": schema.StringAttribute{
						Required: true, Sensitive: true,
						Description: "The fixed code, 6–12 digits. Write-only: the API never returns it, so Terraform trusts its own state.",
					},
				},
			},
		},
	}
}

// reviewLoginArgs decodes the block; a null block clears the sign-in.
func reviewLoginArgs(ctx context.Context, o types.Object) (email, phone, domain, code string) {
	if o.IsNull() || o.IsUnknown() {
		return "", "", "", ""
	}
	var m reviewLoginModel
	if diags := o.As(ctx, &m, basetypes.ObjectAsOptions{}); diags.HasError() {
		return "", "", "", ""
	}
	return m.Email.ValueString(), m.Phone.ValueString(), m.Domain.ValueString(), m.Code.ValueString()
}

// normalizeReviewLogin folds the block the way the server stores it
// (lower-case email and domain, no leading "@"), so the state written
// straight from the plan matches the next Read instead of drifting.
// Phones are stored E.164 server-side; configure them that way.
func normalizeReviewLogin(ctx context.Context, o types.Object) types.Object {
	if o.IsNull() || o.IsUnknown() {
		return o
	}
	email, phone, domain, code := reviewLoginArgs(ctx, o)
	v, _ := types.ObjectValue(reviewLoginAttrTypes, map[string]attr.Value{
		"email":  types.StringValue(strings.ToLower(strings.TrimSpace(email))),
		"phone":  types.StringValue(strings.TrimSpace(phone)),
		"domain": types.StringValue(strings.ToLower(strings.TrimPrefix(strings.TrimSpace(domain), "@"))),
		"code":   types.StringValue(code),
	})
	return v
}

// reviewLoginValue renders what the server holds — null when no principal
// is set, so an omitted block converges. The code never comes back from
// the API: it is carried forward from prior state (a fresh import reads it
// as empty, and the next apply re-sends the configured one).
func reviewLoginValue(ctx context.Context, c *latchkey.OidcClient, prior types.Object) types.Object {
	if c.ReviewEmail == "" && c.ReviewPhone == "" && c.ReviewDomain == "" {
		return types.ObjectNull(reviewLoginAttrTypes)
	}
	code := ""
	if !prior.IsNull() && !prior.IsUnknown() {
		var priorM reviewLoginModel
		if diags := prior.As(ctx, &priorM, basetypes.ObjectAsOptions{}); !diags.HasError() {
			code = priorM.Code.ValueString()
		}
	}
	v, _ := types.ObjectValue(reviewLoginAttrTypes, map[string]attr.Value{
		"email":  types.StringValue(c.ReviewEmail),
		"phone":  types.StringValue(c.ReviewPhone),
		"domain": types.StringValue(c.ReviewDomain),
		"code":   types.StringValue(code),
	})
	return v
}

// exchangeArgs decodes the block into the API call's arguments; a null
// block is the grant switched off.
func exchangeArgs(ctx context.Context, o types.Object) (auds, claims []string, subject bool, ttl int64) {
	if o.IsNull() || o.IsUnknown() {
		return nil, nil, false, 0
	}
	var m exchangeModel
	if diags := o.As(ctx, &m, basetypes.ObjectAsOptions{}); diags.HasError() {
		return nil, nil, false, 0
	}
	return stringList(ctx, m.Audiences), stringList(ctx, m.Claims), m.Subject.ValueBool(), m.TTL.ValueInt64()
}

// exchangeValue renders what the server holds as the block — null when
// the grant is off, so a config that omits the block converges.
func exchangeValue(ctx context.Context, c *latchkey.OidcClient, prior types.Object) types.Object {
	if len(c.ExchangeAudiences) == 0 {
		return types.ObjectNull(exchangeAttrTypes)
	}
	var priorM exchangeModel
	if !prior.IsNull() && !prior.IsUnknown() {
		_ = prior.As(ctx, &priorM, basetypes.ObjectAsOptions{})
	}
	claims := stringListValue(ctx, c.ExchangeClaims, priorM.Claims)
	if len(c.ExchangeClaims) == 0 && (priorM.Claims.IsNull() || priorM.Claims.IsUnknown()) {
		claims = types.ListNull(types.StringType)
	}
	v, _ := types.ObjectValue(exchangeAttrTypes, map[string]attr.Value{
		"audiences": stringListValue(ctx, c.ExchangeAudiences, priorM.Audiences),
		"claims":    claims,
		"subject":   types.BoolValue(c.ExchangeSubject),
		"ttl":       types.Int64Value(c.ExchangeTtl),
	})
	return v
}

func (r *clientResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData != nil {
		r.api = clientFrom(req.ProviderData)
	}
}

func (r *clientResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan clientModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	uris := stringList(ctx, plan.RedirectURIs)
	secretHash := ""
	if !plan.Secret.IsNull() && plan.Secret.ValueString() != "" {
		sum := sha256.Sum256([]byte(plan.Secret.ValueString()))
		secretHash = hex.EncodeToString(sum[:])
	}
	id, err := r.api.RegisterClient(ctx, plan.Name.ValueString(), plan.Public.ValueBool(), uris, secretHash)
	if err != nil {
		resp.Diagnostics.AddError("registering client", err.Error())
		return
	}
	plan.ID = types.StringValue(id)
	if err := r.applySettings(ctx, id, plan, clientModel{}); err != nil {
		resp.Diagnostics.AddError("applying client settings", err.Error())
		// the client exists: record it so a retry reconciles instead of
		// registering a twin
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

// ModifyPlan insists review_login is written the way the server stores
// it (lower-case email and domain, no leading "@", nothing to trim).
// Terraform forbids a plan that differs from config for a set attribute
// and forbids apply from changing a planned value, so the provider
// cannot fold silently — it names the canonical form instead, and the
// state then always matches the next Read.
func (r *clientResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy
	}
	var plan clientModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() || plan.ReviewLogin.IsNull() || plan.ReviewLogin.IsUnknown() {
		return
	}
	email, phone, domain, _ := reviewLoginArgs(ctx, plan.ReviewLogin)
	if email == "" && phone == "" && domain == "" {
		resp.Diagnostics.AddAttributeError(path.Root("review_login"), "review_login needs a principal",
			"Set at least one of email, phone or domain, or omit the block to turn the review sign-in off.")
		return
	}
	folded := normalizeReviewLogin(ctx, plan.ReviewLogin)
	if !folded.Equal(plan.ReviewLogin) {
		var want reviewLoginModel
		_ = folded.As(ctx, &want, basetypes.ObjectAsOptions{})
		resp.Diagnostics.AddAttributeError(path.Root("review_login"), "review_login is not in its stored form",
			fmt.Sprintf("Write the email and domain lower-case with no leading \"@\" and nothing to trim, as the server stores them: email = %q, phone = %q, domain = %q.",
				want.Email.ValueString(), want.Phone.ValueString(), want.Domain.ValueString()))
	}
}

// applySettings pushes the per-flag setters, skipping what matches prior
// state — each flag is its own endpoint and its own event.
func (r *clientResource) applySettings(ctx context.Context, id string, plan, prior clientModel) error {
	if !plan.PhoneEnabled.Equal(prior.PhoneEnabled) && plan.PhoneEnabled.ValueBool() != prior.PhoneEnabled.ValueBool() {
		if err := r.api.SetClientPhone(ctx, id, plan.PhoneEnabled.ValueBool()); err != nil {
			return err
		}
	}
	if plan.CaptchaRequired.ValueBool() != prior.CaptchaRequired.ValueBool() {
		if err := r.api.SetClientCaptcha(ctx, id, plan.CaptchaRequired.ValueBool()); err != nil {
			return err
		}
	}
	if plan.AttestationRequired.ValueBool() != prior.AttestationRequired.ValueBool() {
		if err := r.api.SetClientAttestation(ctx, id, plan.AttestationRequired.ValueBool()); err != nil {
			return err
		}
	}
	if plan.OtpDailyCeiling.ValueInt64() != prior.OtpDailyCeiling.ValueInt64() {
		if err := r.api.SetClientOtpCeiling(ctx, id, plan.OtpDailyCeiling.ValueInt64()); err != nil {
			return err
		}
	}
	// one endpoint for the whole block; the server converges an equal set,
	// so re-sending on any change is cheap and a removed block clears it
	if !plan.Exchange.Equal(prior.Exchange) && !(plan.Exchange.IsNull() && prior.Exchange.IsNull()) {
		auds, claims, subject, ttl := exchangeArgs(ctx, plan.Exchange)
		if err := r.api.SetClientExchange(ctx, id, auds, claims, subject, ttl); err != nil {
			return err
		}
	}
	// same shape: one endpoint, the server converges, a removed block clears
	if !plan.ReviewLogin.Equal(prior.ReviewLogin) && !(plan.ReviewLogin.IsNull() && prior.ReviewLogin.IsNull()) {
		email, phone, domain, code := reviewLoginArgs(ctx, plan.ReviewLogin)
		if err := r.api.SetClientReviewLogin(ctx, id, email, phone, domain, code); err != nil {
			return err
		}
	}
	return nil
}

func (r *clientResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state clientModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	c, err := r.api.GetClient(ctx, state.ID.ValueString())
	if err != nil {
		if latchkey.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("reading client", err.Error())
		return
	}
	state.Name = types.StringValue(c.Name)
	state.Public = types.BoolValue(c.Public)
	state.RedirectURIs = stringListValue(ctx, c.RedirectUris, state.RedirectURIs)
	state.PhoneEnabled = types.BoolValue(c.PhoneEnabled)
	state.CaptchaRequired = types.BoolValue(c.CaptchaRequired)
	state.AttestationRequired = types.BoolValue(c.AttestationRequired)
	state.OtpDailyCeiling = types.Int64Value(c.OtpDailyCeiling)
	state.Exchange = exchangeValue(ctx, c, state.Exchange)
	state.ReviewLogin = reviewLoginValue(ctx, c, state.ReviewLogin)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *clientResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior clientModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := prior.ID.ValueString()
	plan.ID = prior.ID
	if !plan.Name.Equal(prior.Name) {
		if err := r.api.SetClientName(ctx, id, plan.Name.ValueString()); err != nil {
			resp.Diagnostics.AddError("renaming client", err.Error())
			return
		}
	}
	if !plan.RedirectURIs.Equal(prior.RedirectURIs) {
		if err := r.api.SetClientRedirectURIs(ctx, id, stringList(ctx, plan.RedirectURIs)); err != nil {
			resp.Diagnostics.AddError("updating redirect URIs", err.Error())
			return
		}
	}
	if err := r.applySettings(ctx, id, plan, prior); err != nil {
		resp.Diagnostics.AddError("applying client settings", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *clientResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state clientModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.api.DisableClient(ctx, state.ID.ValueString()); err != nil && !latchkey.IsNotFound(err) {
		resp.Diagnostics.AddError("disabling client", err.Error())
	}
}

func (r *clientResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), req.ID)...)
}
