package provider

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// latchkey_federation_target — a tenant's trust at a cloud for Latchkey's
// subject tokens (Latchkey ADR 031 §3): a GCP workload identity provider
// and the service account it may act as, an AWS role, an Azure federated
// credential or a generic OIDC/STS. The target holds no secret. Its PUT
// replaces everything but the grants, so the resource updates in place;
// the target's ref and, for GCP, the principals a trust names are read
// back, so the cloud's side of the trust is declared in the same
// configuration. Grants are their own resource
// (latchkey_federation_target_grant).
type federationTargetResource struct {
	api *latchkey.Client
}

func newFederationTargetResource() resource.Resource { return &federationTargetResource{} }

var (
	_ resource.ResourceWithModifyPlan     = (*federationTargetResource)(nil)
	_ resource.ResourceWithValidateConfig = (*federationTargetResource)(nil)
	_ resource.ResourceWithImportState    = (*federationTargetResource)(nil)
)

type federationTargetModel struct {
	ID                 types.String `tfsdk:"id"`
	Org                types.String `tfsdk:"org"`
	Tenant             types.String `tfsdk:"tenant"`
	Name               types.String `tfsdk:"name"`
	Project            types.String `tfsdk:"project"`
	Cloud              types.String `tfsdk:"cloud"`
	Label              types.String `tfsdk:"label"`
	Description        types.String `tfsdk:"description"`
	Status             types.String `tfsdk:"status"`
	Audience           types.String `tfsdk:"audience"`
	GCP                types.Object `tfsdk:"gcp"`
	AWS                types.Object `tfsdk:"aws"`
	Azure              types.Object `tfsdk:"azure"`
	OIDC               types.Object `tfsdk:"oidc"`
	TargetRef          types.String `tfsdk:"target_ref"`
	Principal          types.String `tfsdk:"principal"`
	PrincipalSet       types.String `tfsdk:"principal_set"`
	AttributeCondition types.String `tfsdk:"attribute_condition"`
}

type federationGCPModel struct {
	WorkloadIdentityProvider types.String `tfsdk:"workload_identity_provider"`
	ServiceAccount           types.String `tfsdk:"service_account"`
	Scopes                   types.Set    `tfsdk:"scopes"`
}

type federationAWSModel struct {
	RoleARN       types.String `tfsdk:"role_arn"`
	Region        types.String `tfsdk:"region"`
	SessionPolicy types.String `tfsdk:"session_policy"`
	MaxDuration   types.Int64  `tfsdk:"max_duration"`
}

type federationAzureModel struct {
	TenantID types.String `tfsdk:"tenant_id"`
	ClientID types.String `tfsdk:"client_id"`
	Scope    types.String `tfsdk:"scope"`
	Hosts    types.Set    `tfsdk:"hosts"`
}

type federationOIDCModel struct {
	TokenURL types.String `tfsdk:"token_url"`
	Grant    types.String `tfsdk:"grant"`
	Hosts    types.Set    `tfsdk:"hosts"`
}

var (
	stringSetType = types.SetType{ElemType: types.StringType}

	federationGCPAttrTypes = map[string]attr.Type{
		"workload_identity_provider": types.StringType,
		"service_account":            types.StringType,
		"scopes":                     stringSetType,
	}
	federationAWSAttrTypes = map[string]attr.Type{
		"role_arn":       types.StringType,
		"region":         types.StringType,
		"session_policy": types.StringType,
		"max_duration":   types.Int64Type,
	}
	federationAzureAttrTypes = map[string]attr.Type{
		"tenant_id": types.StringType,
		"client_id": types.StringType,
		"scope":     types.StringType,
		"hosts":     stringSetType,
	}
	federationOIDCAttrTypes = map[string]attr.Type{
		"token_url": types.StringType,
		"grant":     types.StringType,
		"hosts":     stringSetType,
	}
)

const (
	federationGCPDefaultScope = "https://www.googleapis.com/auth/cloud-platform"
	federationOIDCExchange    = "urn:ietf:params:oauth:grant-type:token-exchange"
	// AWS's bound on a web identity session, the API's default
	federationAWSMaxDuration = 3600
)

// The shapes the API stores. It folds some inputs (a provider's
// //iam.googleapis.com/ prefix, an upper-case GUID or host), and
// Terraform refuses a provider whose applied value differs from its
// plan, so the schema asks for the stored form up front.
var (
	federationNameRe    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	federationGrantRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	federationPurposeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
	federationKindRe    = regexp.MustCompile(`^(\*|[a-z0-9][a-z0-9_-]{0,62})$`)
	federationGCPWIPRe  = regexp.MustCompile(`^projects/[0-9]{1,20}/locations/global/workloadIdentityPools/[a-z0-9-]{4,32}/providers/[a-z0-9-]{4,32}$`)
	federationGCPSARe   = regexp.MustCompile(`^[a-z][a-z0-9-]{4,61}[a-z0-9]@[a-z0-9.-]+\.iam\.gserviceaccount\.com$`)
	federationGUIDRe    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	federationHostRe    = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	federationNoSpaceRe = regexp.MustCompile(`^\S+$`)
	federationTrimmedRe = regexp.MustCompile(`^\S(.*\S)?$`)
)

// federationOrgAttribute is the `org` both federation resources carry:
// the provider's org unless the configuration names another.
func federationOrgAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		Optional: true, Computed: true,
		Description: "Organization slug the tenant belongs to. Defaults to the provider's `org`; the provider's credentials must be allowed to manage the named org.",
		// state first, so an omitted org is not read as a change
		PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplace()},
	}
}

func (r *federationTargetResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_federation_target"
}

func (r *federationTargetResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	hosts := []validator.Set{setvalidator.ValueStringsAre(stringvalidator.RegexMatches(federationHostRe,
		"must be a lower-case host name or *.host name: no scheme, port or path"))}
	resp.Schema = schema.Schema{
		Description: "A tenant's federation target: the trust at a cloud that Latchkey's short-lived subject tokens are traded on " +
			"(a GCP workload identity provider and service account, an AWS role, an Azure federated credential or a generic OIDC/STS). " +
			"It holds no secret. `target_ref` and, for GCP, `principal_set`, `principal` and `attribute_condition` are read back so the " +
			"cloud's side of the trust can be declared in the same configuration. The organization must hold the federation feature. " +
			"Grant it to a product with latchkey_federation_target_grant.",
		Attributes: map[string]schema.Attribute{
			"id":  schema.StringAttribute{Computed: true, Description: "org/tenant/name, with /project for a project's target."},
			"org": federationOrgAttribute(),
			"tenant": schema.StringAttribute{
				Required:      true,
				Description:   "The tenant the target belongs to (its slug or key).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				Required:      true,
				Description:   "The target's name within the tenant: lower-case letters, digits and dashes. Fixed; a deleted target's name stays taken.",
				Validators:    []validator.String{stringvalidator.RegexMatches(federationNameRe, "must be lower-case letters, digits and dashes (at most 63)")},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"project": schema.StringAttribute{
				Optional:      true,
				Description:   "A project's ref, for a target of one project; omit for a target of the whole tenant.",
				Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"cloud": schema.StringAttribute{
				Required:    true,
				Description: "gcp, aws, azure or oidc. Set the block of the same name. A target's cloud never changes: declare another target under a new name.",
				Validators:  []validator.String{stringvalidator.OneOf("gcp", "aws", "azure", "oidc")},
				// with the name unchanged the plan is refused instead
				// (ModifyPlan): a deleted target's name stays taken
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"label": schema.StringAttribute{
				Optional: true, Computed: true,
				Description: "Display name. Defaults to `name`.",
				Validators:  []validator.String{stringvalidator.RegexMatches(federationTrimmedRe, "must not be empty or start or end with whitespace"), stringvalidator.LengthAtMost(200)},
			},
			"description": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "What the target is for.",
				Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^(\S(.*\S)?)?$`), "must not start or end with whitespace"), stringvalidator.LengthAtMost(2000)},
			},
			"status": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString("active"),
				Description: "active or disabled. A disabled target mints nothing and keeps its ref and grants.",
				Validators:  []validator.String{stringvalidator.OneOf("active", "disabled")},
			},
			"audience": schema.StringAttribute{
				Optional: true, Computed: true,
				Description: "The `aud` of the subject tokens. Omitted, it is the cloud's default: for gcp the provider's canonical name " +
					"(https://iam.googleapis.com/<workload_identity_provider>, which a provider with no allowed audiences accepts), " +
					"for aws sts.amazonaws.com, for azure api://AzureADTokenExchange. Required for oidc.",
				Validators: []validator.String{stringvalidator.RegexMatches(federationNoSpaceRe, "must be a single non-blank token"), stringvalidator.LengthAtMost(255)},
			},
			"gcp": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "The GCP trust, for cloud = \"gcp\".",
				Attributes: map[string]schema.Attribute{
					"workload_identity_provider": schema.StringAttribute{
						Required: true,
						Description: "The workload identity pool provider's resource name: " +
							"projects/<number>/locations/global/workloadIdentityPools/<pool>/providers/<provider>. A path, not a reference: " +
							"it can be written before the provider exists.",
						Validators: []validator.String{stringvalidator.RegexMatches(federationGCPWIPRe,
							"must be projects/<number>/locations/global/workloadIdentityPools/<pool>/providers/<provider>, with the project's number and no //iam.googleapis.com/ prefix")},
					},
					"service_account": schema.StringAttribute{
						Optional:    true,
						Description: "The service account the federated token impersonates (its email). Omit to use the federated token alone.",
						Validators: []validator.String{stringvalidator.RegexMatches(federationGCPSARe,
							"must be a service account's email, lower-case (<name>@<project>.iam.gserviceaccount.com)")},
					},
					"scopes": schema.SetAttribute{
						ElementType: types.StringType,
						Optional:    true, Computed: true,
						Default:     setdefault.StaticValue(types.SetValueMust(types.StringType, []attr.Value{types.StringValue(federationGCPDefaultScope)})),
						Description: "OAuth scopes of the access token. Defaults to " + federationGCPDefaultScope + ".",
						Validators: []validator.Set{setvalidator.SizeAtLeast(1), setvalidator.ValueStringsAre(stringvalidator.RegexMatches(
							regexp.MustCompile(`^https://www\.googleapis\.com/auth/\S+$`), "must be a Google OAuth scope (https://www.googleapis.com/auth/…)"))},
					},
				},
			},
			"aws": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "The AWS trust, for cloud = \"aws\": the role AssumeRoleWithWebIdentity assumes.",
				Attributes: map[string]schema.Attribute{
					"role_arn": schema.StringAttribute{
						Required:    true,
						Description: "The IAM role's ARN (arn:aws:iam::<account>:role/<name>).",
						Validators: []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws[a-z-]*:iam::[0-9]{12}:role/[\w+=,.@/-]{1,512}$`),
							"must be an IAM role's ARN")},
					},
					"region": schema.StringAttribute{
						Required:    true,
						Description: "The region whose STS endpoint is called (us-east-1).",
						Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`), "must be an AWS region")},
					},
					"session_policy": schema.StringAttribute{
						Optional:    true,
						Description: "An optional session policy (a JSON policy document, at most 2048 bytes) narrowing the role.",
						Validators:  []validator.String{stringvalidator.RegexMatches(federationTrimmedRe, "must not be empty or start or end with whitespace"), stringvalidator.LengthAtMost(2048)},
					},
					"max_duration": schema.Int64Attribute{
						Optional: true, Computed: true, Default: int64default.StaticInt64(federationAWSMaxDuration),
						Description: "The session's length in seconds, 900–3600. Defaults to 3600.",
						Validators:  []validator.Int64{int64validator.Between(900, federationAWSMaxDuration)},
					},
				},
			},
			"azure": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "The Azure trust, for cloud = \"azure\": a federated credential on an app registration or managed identity.",
				Attributes: map[string]schema.Attribute{
					"tenant_id": schema.StringAttribute{
						Required:    true,
						Description: "The directory (Entra tenant) id, a lower-case GUID.",
						Validators:  []validator.String{stringvalidator.RegexMatches(federationGUIDRe, "must be a lower-case GUID")},
					},
					"client_id": schema.StringAttribute{
						Required:    true,
						Description: "The app registration's or managed identity's client id, a lower-case GUID.",
						Validators:  []validator.String{stringvalidator.RegexMatches(federationGUIDRe, "must be a lower-case GUID")},
					},
					"scope": schema.StringAttribute{
						Required:    true,
						Description: "The scope the token is for (https://management.azure.com/.default).",
						Validators:  []validator.String{stringvalidator.RegexMatches(federationTrimmedRe, "must not be empty or start or end with whitespace")},
					},
					"hosts": schema.SetAttribute{
						ElementType: types.StringType,
						Optional:    true,
						Description: "The hosts a job's relay writes the token on. Omitted, they are the scope's resource host; required when the scope names none (api://<app>/.default).",
						Validators:  append([]validator.Set{setvalidator.SizeBetween(1, 16)}, hosts...),
					},
				},
			},
			"oidc": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "A generic OIDC trust, for cloud = \"oidc\". `audience` is required.",
				Attributes: map[string]schema.Attribute{
					"token_url": schema.StringAttribute{
						Optional: true,
						Description: "The RFC 8693 (or a vendor's) token endpoint the subject token is traded at, an https URL. " +
							"Omit for a direct target: the service at `hosts` takes the subject token itself as a bearer.",
						Validators: []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^https://[^\s#@]+$`), "must be an https URL with no user or fragment")},
					},
					"grant": schema.StringAttribute{
						Optional: true, Computed: true,
						Description: "The grant type sent to `token_url`. Defaults to " + federationOIDCExchange + " when `token_url` is set; a direct target names none.",
						Validators:  []validator.String{stringvalidator.RegexMatches(federationNoSpaceRe, "must be a single non-blank token")},
					},
					"hosts": schema.SetAttribute{
						ElementType: types.StringType,
						Required:    true,
						Description: "The hosts a job's relay writes the token on (lower-case host names, `*.` allowed; at most 16).",
						Validators:  append([]validator.Set{setvalidator.SizeBetween(1, 16)}, hosts...),
					},
				},
			},
			"target_ref": schema.StringAttribute{
				Computed: true,
				Description: "The target's opaque ref: what products store, and what a subject token's `target` claim and `sub` " +
					"(fed:<target_ref>:<product>:<kind|workload>) name. Stable for the target's life.",
			},
			"principal_set": schema.StringAttribute{
				Computed: true,
				Description: "gcp only: the principalSet:// member matching every subject token for this target " +
					"(the pool's attribute.target/<target_ref>), given the attribute mapping attribute.target = assertion.target. " +
					"Grant it roles/iam.workloadIdentityUser on the service account. Null for other clouds.",
			},
			"principal": schema.StringAttribute{
				Computed: true,
				Description: "gcp only: the principal:// member for one subject, as a pattern — it ends " +
					"fed:<target_ref>:<product>:<kind|workload>, with those two placeholders left for you to fill. Null for other clouds.",
			},
			"attribute_condition": schema.StringAttribute{
				Computed: true,
				Description: "gcp only: the attribute condition Latchkey recommends for the workload identity pool provider — " +
					"assertion.tenant == \"<the tenant's ref>\" && assertion.target == \"<target_ref>\". Null for other clouds.",
			},
		},
	}
}

func (r *federationTargetResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData != nil {
		r.api = clientFrom(req.ProviderData)
	}
}

// ValidateConfig holds the configuration to one cloud: the block `cloud`
// names, and no other.
func (r *federationTargetResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg federationTargetModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() || cfg.Cloud.IsUnknown() || cfg.Cloud.IsNull() {
		return
	}
	cloud := cfg.Cloud.ValueString()
	for name, block := range map[string]types.Object{"gcp": cfg.GCP, "aws": cfg.AWS, "azure": cfg.Azure, "oidc": cfg.OIDC} {
		switch {
		case block.IsUnknown():
		case name == cloud && block.IsNull():
			resp.Diagnostics.AddAttributeError(path.Root(name), "the target's cloud block is missing",
				fmt.Sprintf("cloud = %q needs the `%s` block.", cloud, name))
		case name != cloud && !block.IsNull():
			resp.Diagnostics.AddAttributeError(path.Root(name), "a target names only its own cloud's fields",
				fmt.Sprintf("cloud = %q takes the `%s` block only; remove `%s`.", cloud, cloud, name))
		}
	}
	if cloud == "oidc" && cfg.Audience.IsNull() {
		resp.Diagnostics.AddAttributeError(path.Root("audience"), "an oidc target names its audience",
			"Set `audience`: an oidc target has no default.")
	}
	if cloud == "oidc" && !cfg.OIDC.IsNull() && !cfg.OIDC.IsUnknown() {
		var o federationOIDCModel
		if diags := cfg.OIDC.As(ctx, &o, basetypes.ObjectAsOptions{}); !diags.HasError() && o.TokenURL.IsNull() && !o.Grant.IsNull() && !o.Grant.IsUnknown() {
			resp.Diagnostics.AddAttributeError(path.Root("oidc").AtName("grant"), "a grant is a token_url's",
				"A direct oidc target (no `token_url`) names no `grant`.")
		}
	}
}

// federationDefaultAudience is the aud the API reports for a target with
// no audience of its own; ok is false when it is not yet knowable.
func federationDefaultAudience(ctx context.Context, m federationTargetModel) (string, bool) {
	switch m.Cloud.ValueString() {
	case "gcp":
		if m.GCP.IsNull() || m.GCP.IsUnknown() {
			return "", false
		}
		var g federationGCPModel
		if diags := m.GCP.As(ctx, &g, basetypes.ObjectAsOptions{}); diags.HasError() || g.WorkloadIdentityProvider.IsNull() || g.WorkloadIdentityProvider.IsUnknown() {
			return "", false
		}
		return "https://iam.googleapis.com/" + g.WorkloadIdentityProvider.ValueString(), true
	case "aws":
		return "sts.amazonaws.com", true
	case "azure":
		return "api://AzureADTokenExchange", true
	}
	return "", false
}

func federationGCPProvider(ctx context.Context, o types.Object) types.String {
	if o.IsNull() || o.IsUnknown() {
		return types.StringNull()
	}
	var g federationGCPModel
	if diags := o.As(ctx, &g, basetypes.ObjectAsOptions{}); diags.HasError() {
		return types.StringNull()
	}
	return g.WorkloadIdentityProvider
}

// ModifyPlan fills the values the API would default (the org, the label,
// the audience, an oidc exchange's grant), so the plan shows them and the
// applied state matches it, and carries the read-back values forward
// while what they derive from is unchanged.
func (r *federationTargetResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy
	}
	var plan, cfg federationTargetModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if plan.Org.IsUnknown() && cfg.Org.IsNull() && r.api != nil {
		plan.Org = types.StringValue(r.api.Org)
	}
	if cfg.Label.IsNull() {
		plan.Label = plan.Name
	}
	if cfg.Audience.IsNull() {
		plan.Audience = types.StringUnknown()
		if aud, ok := federationDefaultAudience(ctx, plan); ok {
			plan.Audience = types.StringValue(aud)
		}
	}
	if !plan.OIDC.IsNull() && !plan.OIDC.IsUnknown() {
		var o federationOIDCModel
		if diags := plan.OIDC.As(ctx, &o, basetypes.ObjectAsOptions{}); !diags.HasError() && o.Grant.IsUnknown() && !o.TokenURL.IsUnknown() {
			o.Grant = types.StringValue("")
			if o.TokenURL.ValueString() != "" {
				o.Grant = types.StringValue(federationOIDCExchange)
			}
			v, diags := types.ObjectValueFrom(ctx, federationOIDCAttrTypes, o)
			resp.Diagnostics.Append(diags...)
			plan.OIDC = v
		}
	}

	// the read-back values: unknown for a new target, the state's while
	// the target and what they derive from stay as they are
	plan.ID, plan.TargetRef = types.StringUnknown(), types.StringUnknown()
	plan.Principal, plan.PrincipalSet, plan.AttributeCondition = types.StringUnknown(), types.StringUnknown(), types.StringUnknown()
	if !plan.Cloud.IsUnknown() && plan.Cloud.ValueString() != "gcp" {
		plan.Principal, plan.PrincipalSet, plan.AttributeCondition = types.StringNull(), types.StringNull(), types.StringNull()
	}
	if !req.State.Raw.IsNull() {
		var state federationTargetModel
		resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
		if resp.Diagnostics.HasError() {
			return
		}
		same := plan.Org.Equal(state.Org) && plan.Tenant.Equal(state.Tenant) && plan.Name.Equal(state.Name) && plan.Project.Equal(state.Project)
		if same && !plan.Cloud.IsUnknown() && !plan.Cloud.Equal(state.Cloud) {
			resp.Diagnostics.AddAttributeError(path.Root("cloud"), "a federation target's cloud never changes",
				fmt.Sprintf("Target %q is a %s target. Latchkey never changes a target's cloud, and a deleted target's name stays taken, "+
					"so the provider cannot replace it under the same name: declare the %s target under a new `name`.",
					state.Name.ValueString(), state.Cloud.ValueString(), plan.Cloud.ValueString()))
			return
		}
		if same {
			plan.ID, plan.TargetRef = state.ID, state.TargetRef
			if federationGCPProvider(ctx, plan.GCP).Equal(federationGCPProvider(ctx, state.GCP)) {
				plan.Principal, plan.PrincipalSet, plan.AttributeCondition = state.Principal, state.PrincipalSet, state.AttributeCondition
			}
		}
	}
	resp.Diagnostics.Append(resp.Plan.Set(ctx, plan)...)
}

// federationID is the resource id, and the import id: org/tenant/name,
// with /project for a project's target.
func federationID(org, tenant, name, project string) string {
	id := org + "/" + tenant + "/" + name
	if project != "" {
		id += "/" + project
	}
	return id
}

// input builds the PUT's body from the plan. The audience is the
// configuration's: "" keeps the cloud's default, where the plan holds
// the default's value.
func (r *federationTargetResource) input(ctx context.Context, plan federationTargetModel, cfgAudience types.String) (latchkey.FederationTargetInput, error) {
	label, description, status := plan.Label.ValueString(), plan.Description.ValueString(), plan.Status.ValueString()
	in := latchkey.FederationTargetInput{
		Cloud: plan.Cloud.ValueString(), Audience: cfgAudience.ValueString(),
		Label: &label, Description: &description, Status: &status,
	}
	opts := basetypes.ObjectAsOptions{}
	switch in.Cloud {
	case "gcp":
		var g federationGCPModel
		if diags := plan.GCP.As(ctx, &g, opts); diags.HasError() {
			return in, fmt.Errorf("reading the gcp block")
		}
		in.GCP = &latchkey.FederationGCP{WorkloadIdentityProvider: g.WorkloadIdentityProvider.ValueString(),
			ServiceAccount: g.ServiceAccount.ValueString(), Scopes: stringSet(ctx, g.Scopes)}
	case "aws":
		var a federationAWSModel
		if diags := plan.AWS.As(ctx, &a, opts); diags.HasError() {
			return in, fmt.Errorf("reading the aws block")
		}
		in.AWS = &latchkey.FederationAWS{RoleARN: a.RoleARN.ValueString(), Region: a.Region.ValueString(),
			SessionPolicy: a.SessionPolicy.ValueString(), MaxDuration: a.MaxDuration.ValueInt64()}
	case "azure":
		var a federationAzureModel
		if diags := plan.Azure.As(ctx, &a, opts); diags.HasError() {
			return in, fmt.Errorf("reading the azure block")
		}
		in.Azure = &latchkey.FederationAzure{TenantID: a.TenantID.ValueString(), ClientID: a.ClientID.ValueString(),
			Scope: a.Scope.ValueString(), Hosts: stringSet(ctx, a.Hosts)}
	case "oidc":
		var o federationOIDCModel
		if diags := plan.OIDC.As(ctx, &o, opts); diags.HasError() {
			return in, fmt.Errorf("reading the oidc block")
		}
		in.OIDC = &latchkey.FederationOIDC{TokenURL: o.TokenURL.ValueString(), Grant: o.Grant.ValueString(), Hosts: stringSet(ctx, o.Hosts)}
	}
	return in, nil
}

// fill writes what the API answered into the model. prior is the model
// before the call: an optional value the API reports empty stays null
// where the configuration never set it.
func (r *federationTargetResource) fill(ctx context.Context, m *federationTargetModel, t *latchkey.FederationTarget) {
	prior := *m
	m.ID = types.StringValue(federationID(m.Org.ValueString(), m.Tenant.ValueString(), m.Name.ValueString(), m.Project.ValueString()))
	m.Project = stringOrNull(t.Project, prior.Project)
	m.Cloud = types.StringValue(t.Cloud)
	m.Label = types.StringValue(t.Label)
	m.Description = types.StringValue(t.Description)
	m.Status = types.StringValue(t.Status)
	m.Audience = types.StringValue(t.Audience)
	m.TargetRef = types.StringValue(t.TargetRef)

	m.GCP, m.AWS = types.ObjectNull(federationGCPAttrTypes), types.ObjectNull(federationAWSAttrTypes)
	m.Azure, m.OIDC = types.ObjectNull(federationAzureAttrTypes), types.ObjectNull(federationOIDCAttrTypes)
	m.Principal, m.PrincipalSet, m.AttributeCondition = types.StringNull(), types.StringNull(), types.StringNull()
	opts := basetypes.ObjectAsOptions{}
	switch {
	case t.Cloud == "gcp" && t.GCP != nil:
		var p federationGCPModel
		if !prior.GCP.IsNull() && !prior.GCP.IsUnknown() {
			_ = prior.GCP.As(ctx, &p, opts)
		}
		scopes, _ := types.SetValueFrom(ctx, types.StringType, t.GCP.Scopes)
		m.GCP, _ = types.ObjectValue(federationGCPAttrTypes, map[string]attr.Value{
			"workload_identity_provider": types.StringValue(t.GCP.WorkloadIdentityProvider),
			"service_account":            stringOrNull(t.GCP.ServiceAccount, nullIfUnset(p.ServiceAccount)),
			"scopes":                     scopes,
		})
		if s := t.Setup.GCP; s != nil {
			m.Principal, m.PrincipalSet = types.StringValue(s.Principal), types.StringValue(s.PrincipalSet)
			m.AttributeCondition = types.StringValue(s.AttributeCondition)
		}
	case t.Cloud == "aws" && t.AWS != nil:
		var p federationAWSModel
		if !prior.AWS.IsNull() && !prior.AWS.IsUnknown() {
			_ = prior.AWS.As(ctx, &p, opts)
		}
		m.AWS, _ = types.ObjectValue(federationAWSAttrTypes, map[string]attr.Value{
			"role_arn":       types.StringValue(t.AWS.RoleARN),
			"region":         types.StringValue(t.AWS.Region),
			"session_policy": stringOrNull(t.AWS.SessionPolicy, nullIfUnset(p.SessionPolicy)),
			"max_duration":   types.Int64Value(t.AWS.MaxDuration),
		})
	case t.Cloud == "azure" && t.Azure != nil:
		var p federationAzureModel
		if !prior.Azure.IsNull() && !prior.Azure.IsUnknown() {
			_ = prior.Azure.As(ctx, &p, opts)
		}
		m.Azure, _ = types.ObjectValue(federationAzureAttrTypes, map[string]attr.Value{
			"tenant_id": types.StringValue(t.Azure.TenantID),
			"client_id": types.StringValue(t.Azure.ClientID),
			"scope":     types.StringValue(t.Azure.Scope),
			"hosts":     stringSetValue(ctx, t.Azure.Hosts, setNullIfUnset(p.Hosts)),
		})
	case t.Cloud == "oidc" && t.OIDC != nil:
		var p federationOIDCModel
		if !prior.OIDC.IsNull() && !prior.OIDC.IsUnknown() {
			_ = prior.OIDC.As(ctx, &p, opts)
		}
		hosts, _ := types.SetValueFrom(ctx, types.StringType, t.OIDC.Hosts)
		m.OIDC, _ = types.ObjectValue(federationOIDCAttrTypes, map[string]attr.Value{
			"token_url": stringOrNull(t.OIDC.TokenURL, nullIfUnset(p.TokenURL)),
			"grant":     types.StringValue(t.OIDC.Grant),
			"hosts":     hosts,
		})
	}
}

// nullIfUnset reads a zero or unknown prior as null, so stringOrNull
// keeps an unset optional null.
func nullIfUnset(v types.String) types.String {
	if v.IsUnknown() || v.IsNull() {
		return types.StringNull()
	}
	return v
}

func setNullIfUnset(v types.Set) types.Set {
	if v.IsUnknown() || v.IsNull() {
		return types.SetNull(types.StringType)
	}
	return v
}

// federationError names the two refusals a plain 404 or 409 hides.
func federationError(err error, org, tenant string) string {
	if api, ok := latchkey.AsAPIError(err); ok {
		switch api.Status {
		case http.StatusNotFound:
			return err.Error() + fmt.Sprintf(" — tenant %q of organization %q does not exist, or the organization does not hold the federation feature (every federation route then answers 404).", tenant, org)
		case http.StatusConflict:
			return err.Error() + " — deleting a federation target is final and its name stays taken: declare the target under a new `name`."
		}
	}
	return err.Error()
}

func (r *federationTargetResource) put(ctx context.Context, plan *federationTargetModel, cfgAudience types.String) error {
	in, err := r.input(ctx, *plan, cfgAudience)
	if err != nil {
		return err
	}
	t, err := r.api.SetFederationTarget(ctx, plan.Org.ValueString(), plan.Tenant.ValueString(), plan.Name.ValueString(), plan.Project.ValueString(), in)
	if err != nil {
		return fmt.Errorf("%s", federationError(err, plan.Org.ValueString(), plan.Tenant.ValueString()))
	}
	r.fill(ctx, plan, t)
	return nil
}

func (r *federationTargetResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan federationTargetModel
	var audience types.String
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("audience"), &audience)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.put(ctx, &plan, audience); err != nil {
		resp.Diagnostics.AddError("setting federation target", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *federationTargetResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state federationTargetModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	t, err := r.api.GetFederationTarget(ctx, state.Org.ValueString(), state.Tenant.ValueString(), state.Name.ValueString(), state.Project.ValueString())
	if err != nil {
		// deleted, or its tenant is gone — and a deleted target's name
		// stays taken, so the next apply says so rather than recreating
		if latchkey.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("reading federation target", err.Error())
		return
	}
	r.fill(ctx, &state, t)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *federationTargetResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan federationTargetModel
	var audience types.String
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("audience"), &audience)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.put(ctx, &plan, audience); err != nil {
		resp.Diagnostics.AddError("updating federation target", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *federationTargetResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state federationTargetModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	err := r.api.DeleteFederationTarget(ctx, state.Org.ValueString(), state.Tenant.ValueString(), state.Name.ValueString(), state.Project.ValueString())
	if err != nil && !latchkey.IsNotFound(err) {
		resp.Diagnostics.AddError("deleting federation target", err.Error())
	}
}

// ImportState reads org/tenant/name, or org/tenant/name/project for a
// project's target.
func (r *federationTargetResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// a project's ref may itself hold slashes: it is the rest
	parts := strings.SplitN(req.ID, "/", 4)
	if len(parts) < 3 || slicesHasEmpty(parts) {
		resp.Diagnostics.AddError("unexpected import id", fmt.Sprintf("Import a federation target by org/tenant/name (or org/tenant/name/project for a project's target); got %q.", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("org"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("tenant"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("name"), parts[2])...)
	if len(parts) == 4 {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("project"), parts[3])...)
	}
}

func slicesHasEmpty(parts []string) bool {
	for _, p := range parts {
		if p == "" {
			return true
		}
	}
	return false
}
