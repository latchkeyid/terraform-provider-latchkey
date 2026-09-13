package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
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
		},
	}
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
