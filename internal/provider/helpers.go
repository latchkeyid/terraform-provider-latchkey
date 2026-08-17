package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func pathRoot(name string) path.Path { return path.Root(name) }

func boolRequiresReplace() planmodifier.Bool { return boolplanmodifier.RequiresReplace() }

// stringList unwraps a types.List of strings; null/unknown lists come
// back empty.
func stringList(ctx context.Context, l types.List) []string {
	if l.IsNull() || l.IsUnknown() {
		return nil
	}
	out := make([]string, 0, len(l.Elements()))
	l.ElementsAs(ctx, &out, false)
	return out
}

// stringListValue wraps a []string, preserving null when the prior value
// was null and the service echoes empty — so an unset optional list does
// not flap between null and [].
func stringListValue(ctx context.Context, v []string, prior types.List) types.List {
	if len(v) == 0 && prior.IsNull() {
		return types.ListNull(types.StringType)
	}
	out, _ := types.ListValueFrom(ctx, types.StringType, v)
	return out
}

// stringOrNull mirrors the same rule for scalar strings: an empty answer
// stays null when the config never set the attribute.
func stringOrNull(v string, prior types.String) types.String {
	if v == "" && prior.IsNull() {
		return types.StringNull()
	}
	return types.StringValue(v)
}
