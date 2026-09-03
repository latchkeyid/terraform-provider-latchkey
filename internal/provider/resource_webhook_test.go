package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// the secret must be a sensitive attribute: terraform redacts it from
// plan output and marks it in state
func TestWebhookSecretIsSensitive(t *testing.T) {
	var resp resource.SchemaResponse
	newWebhookResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if !resp.Schema.Attributes["secret"].IsSensitive() {
		t.Fatal("latchkey_webhook.secret must be sensitive")
	}
	if resp.Schema.Attributes["url"].IsSensitive() {
		t.Fatal("url is not a secret")
	}
}
