package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// latchkey_mail_template — one customer-authored email body per kind
// (login, invite). The service injects the link; a template can never
// rewrite it. Destroy restores the platform's default copy.
type mailTemplateResource struct {
	api *latchkey.Client
}

func newMailTemplateResource() resource.Resource { return &mailTemplateResource{} }

type mailTemplateModel struct {
	ID      types.String `tfsdk:"id"`
	Kind    types.String `tfsdk:"kind"`
	Subject types.String `tfsdk:"subject"`
	Body    types.String `tfsdk:"body"`
	HTML    types.String `tfsdk:"html"`
}

func (r *mailTemplateResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mail_template"
}

func (r *mailTemplateResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A per-org email template. The service injects the sign-in link — templates cannot rewrite it. Destroy restores the default copy.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, Description: "The template kind."},
			"kind": schema.StringAttribute{
				Required:      true,
				Description:   "Which email this template dresses: login or invite.",
				Validators:    []validator.String{stringvalidator.OneOf("login", "invite")},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"subject": schema.StringAttribute{Required: true, Description: "Subject line."},
			"body":    schema.StringAttribute{Required: true, Description: "Plaintext body. Variables per the org API's allowlist; {{.Link}} is injected by the service."},
			"html":    schema.StringAttribute{Optional: true, Description: "Optional branded HTML alternative part; the plaintext body always rides along."},
		},
	}
}

func (r *mailTemplateResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData != nil {
		r.api = clientFrom(req.ProviderData)
	}
}

func (r *mailTemplateResource) upsert(ctx context.Context, plan *mailTemplateModel) error {
	if err := r.api.SetTemplate(ctx, latchkey.MailTemplate{
		Kind:    plan.Kind.ValueString(),
		Subject: plan.Subject.ValueString(),
		Body:    plan.Body.ValueString(),
		HTML:    plan.HTML.ValueString(),
	}); err != nil {
		return err
	}
	plan.ID = plan.Kind
	return nil
}

func (r *mailTemplateResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan mailTemplateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.upsert(ctx, &plan); err != nil {
		resp.Diagnostics.AddError("setting template", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *mailTemplateResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state mailTemplateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	t, err := r.api.GetTemplate(ctx, state.Kind.ValueString())
	if err != nil {
		if latchkey.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("reading template", err.Error())
		return
	}
	state.ID = state.Kind
	state.Subject = types.StringValue(t.Subject)
	state.Body = types.StringValue(t.Body)
	state.HTML = stringOrNull(t.HTML, state.HTML)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *mailTemplateResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan mailTemplateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.upsert(ctx, &plan); err != nil {
		resp.Diagnostics.AddError("updating template", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *mailTemplateResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state mailTemplateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.api.ClearTemplate(ctx, state.Kind.ValueString()); err != nil && !latchkey.IsNotFound(err) {
		resp.Diagnostics.AddError("clearing template", err.Error())
	}
}
