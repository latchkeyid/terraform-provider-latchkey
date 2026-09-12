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

// latchkey_auth_domain — a product-branded issuer host claimed by the
// org (auth.<product-domain>). The DNS record and, on Cloud Run, the
// domain mapping stay infrastructure concerns for other providers; this
// claims the host in Latchkey's registry so it serves the org's login.
type authDomainResource struct {
	api *latchkey.Client
}

func newAuthDomainResource() resource.Resource { return &authDomainResource{} }

type authDomainModel struct {
	ID     types.String `tfsdk:"id"`
	Domain types.String `tfsdk:"domain"`
	Issuer types.String `tfsdk:"issuer"`
	RpId   types.String `tfsdk:"rp_id"`
}

func (r *authDomainResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_auth_domain"
}

func (r *authDomainResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A product-branded issuer host claimed by the org. Pair with your DNS provider's record and the platform's domain mapping.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, Description: "The domain."},
			"domain": schema.StringAttribute{
				Required:      true,
				Description:   "The auth host, e.g. auth.example.com. Changing it releases and claims anew.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"issuer": schema.StringAttribute{
				Computed:    true,
				Description: "The issuer URL this domain serves.",
			},
			"rp_id": schema.StringAttribute{
				Optional:    true,
				Description: "Passkey (WebAuthn) scope for this host: the domain itself when unset, or a parent of it (e.g. example.com for auth.example.com) so passkeys already enrolled against the apex keep working. Changing it strands passkeys enrolled under the previous value.",
			},
		},
	}
}

func (r *authDomainResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData != nil {
		r.api = clientFrom(req.ProviderData)
	}
}

func (r *authDomainResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan authDomainModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.api.ClaimAuthDomain(ctx, plan.Domain.ValueString(), plan.RpId.ValueString()); err != nil {
		resp.Diagnostics.AddError("claiming auth domain", err.Error())
		return
	}
	plan.ID = plan.Domain
	plan.Issuer = types.StringValue("https://" + plan.Domain.ValueString())
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *authDomainResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state authDomainModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	items, err := r.api.AuthDomains(ctx)
	if err != nil {
		resp.Diagnostics.AddError("listing auth domains", err.Error())
		return
	}
	for _, d := range items {
		if d.Domain == state.Domain.ValueString() {
			state.ID = state.Domain
			state.Issuer = types.StringValue(d.Issuer)
			// "" upstream means the default (the domain itself); keep an
			// unset attribute null so an omitted rp_id stays a no-op plan.
			if d.RpId == "" {
				state.RpId = types.StringNull()
			} else {
				state.RpId = types.StringValue(d.RpId)
			}
			resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
			return
		}
	}
	resp.State.RemoveResource(ctx)
}

func (r *authDomainResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// domain forces replacement; the only in-place change is rp_id, and a
	// re-claim by the same org is how the server takes it
	var plan authDomainModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.api.ClaimAuthDomain(ctx, plan.Domain.ValueString(), plan.RpId.ValueString()); err != nil {
		resp.Diagnostics.AddError("updating auth domain", err.Error())
		return
	}
	plan.ID = plan.Domain
	plan.Issuer = types.StringValue("https://" + plan.Domain.ValueString())
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *authDomainResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state authDomainModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.api.ReleaseAuthDomain(ctx, state.Domain.ValueString()); err != nil && !latchkey.IsNotFound(err) {
		resp.Diagnostics.AddError("releasing auth domain", err.Error())
	}
}
