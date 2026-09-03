package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// latchkey_webhook — one signed webhook endpoint of the org. The service
// keys endpoints by (org, url): registering the same url again rotates
// the secret and event filter in place, so url changes replace the
// resource while secret/events update it. Deliveries, retries and
// redelivery are runtime state and stay in the console — so do
// invitations, which this provider deliberately never models.
type webhookResource struct {
	api *latchkey.Client
}

func newWebhookResource() resource.Resource { return &webhookResource{} }

type webhookModel struct {
	ID     types.String `tfsdk:"id"`
	URL    types.String `tfsdk:"url"`
	Secret types.String `tfsdk:"secret"`
	Events types.List   `tfsdk:"events"`
	Status types.String `tfsdk:"status"`
}

func (r *webhookResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_webhook"
}

func (r *webhookResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A signed webhook endpoint. Every delivery carries X-Latchkey-Signature (hex HMAC-SHA256 of the " +
			"raw body under the secret), X-Latchkey-Event and X-Latchkey-Delivery. Endpoints are keyed by url: " +
			"changing the url replaces the endpoint, changing the secret or events rotates it in place. " +
			"Runtime deliveries (and the invitations they announce) stay in the console.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, Description: "The endpoint url.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"url": schema.StringAttribute{
				Required:      true,
				Description:   "Absolute https URL that receives deliveries.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"secret": schema.StringAttribute{
				Required:    true,
				Sensitive:   true,
				Description: "Shared HMAC secret. Write-only on the service side — never read back; a changed value re-registers the endpoint.",
			},
			"events": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Event filter (e.g. invitation.*, membership.set). Unset delivers every event type in the catalog.",
			},
			"status": schema.StringAttribute{
				Computed:      true,
				Description:   "active while the endpoint is registered; a disabled endpoint drops out of state.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *webhookResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData != nil {
		r.api = clientFrom(req.ProviderData)
	}
}

func (r *webhookResource) set(ctx context.Context, plan *webhookModel) error {
	if err := r.api.SetWebhook(ctx, plan.URL.ValueString(), plan.Secret.ValueString(), stringList(ctx, plan.Events)); err != nil {
		return err
	}
	plan.ID = plan.URL
	plan.Status = types.StringValue("active")
	return nil
}

func (r *webhookResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan webhookModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.set(ctx, &plan); err != nil {
		resp.Diagnostics.AddError("registering webhook", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *webhookResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state webhookModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	wh, err := r.api.GetWebhook(ctx, state.URL.ValueString())
	if err != nil {
		// disabled from the console or never registered — recreate on
		// the next apply
		if latchkey.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("reading webhook", err.Error())
		return
	}
	state.ID = state.URL
	state.Events = stringListValue(ctx, wh.Events, state.Events)
	state.Status = types.StringValue("active")
	// the secret is write-only; state keeps what was last applied
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *webhookResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan webhookModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.set(ctx, &plan); err != nil {
		resp.Diagnostics.AddError("updating webhook", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *webhookResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state webhookModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.api.DisableWebhook(ctx, state.URL.ValueString()); err != nil && !latchkey.IsNotFound(err) {
		resp.Diagnostics.AddError("disabling webhook", err.Error())
	}
}
