package provider

// The stub's federation targets: Latchkey's
// /org/{org}/tenants/{tenant}/federation-targets routes
// (oidc/federationtarget.go) with the aggregate's rules
// (identity/aggregates/federationtarget.go) — the PUT replaces all but
// the grants and keeps a nil label, description or status; the cloud
// never changes; a deleted target answers 404 and its name stays taken.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"
)

type stubFedGrant struct {
	Product      string   `json:"product"`
	Purposes     []string `json:"purposes"`
	Kinds        []string `json:"kinds"`
	Environments []string `json:"environments"`
	GrantedBy    string   `json:"granted_by"`
}

type stubFedTarget struct {
	Ref, Tenant, Project, Name string
	Label, Description         string
	Cloud, Audience, Status    string
	// gcp
	WIP, ServiceAccount string
	Scopes              []string
	// aws
	RoleARN, Region, SessionPolicy string
	MaxDuration                    int64
	// azure
	AzureTenantID, ClientID, Scope string
	// oidc
	TokenURL, Grant string
	// azure and oidc
	Hosts  []string
	Grants []stubFedGrant
}

// what PUT …/federation-targets/{name} takes; unknown fields are refused
type stubFedBody struct {
	Label       *string `json:"label"`
	Description *string `json:"description"`
	Cloud       string  `json:"cloud"`
	Audience    string  `json:"audience"`
	Status      *string `json:"status"`
	GCP         *struct {
		WorkloadIdentityProvider string   `json:"workload_identity_provider"`
		ServiceAccount           string   `json:"service_account"`
		Scopes                   []string `json:"scopes"`
	} `json:"gcp"`
	AWS *struct {
		RoleARN       string `json:"role_arn"`
		Region        string `json:"region"`
		SessionPolicy string `json:"session_policy"`
		MaxDuration   int64  `json:"max_duration"`
	} `json:"aws"`
	Azure *struct {
		TenantID string   `json:"tenant_id"`
		ClientID string   `json:"client_id"`
		Scope    string   `json:"scope"`
		Hosts    []string `json:"hosts"`
	} `json:"azure"`
	OIDC *struct {
		TokenURL string   `json:"token_url"`
		Grant    string   `json:"grant"`
		Hosts    []string `json:"hosts"`
	} `json:"oidc"`
}

var (
	stubFedNameRe  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	stubFedGrantRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	stubFedWIPRe   = regexp.MustCompile(`^projects/[0-9]{1,20}/locations/global/workloadIdentityPools/[a-z0-9-]{4,32}/providers/[a-z0-9-]{4,32}$`)
	stubFedGUIDRe  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

const stubIssuer = "https://auth.latchkey.test"

func stubFedAudience(t *stubFedTarget) string {
	if t.Audience != "" {
		return t.Audience
	}
	switch t.Cloud {
	case "gcp":
		return "https://iam.googleapis.com/" + t.WIP
	case "aws":
		return "sts.amazonaws.com"
	case "azure":
		return "api://AzureADTokenExchange"
	}
	return ""
}

func stubFedSorted(in []string) []string {
	out := []string{}
	for _, v := range in {
		if v = strings.TrimSpace(v); !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// row is the target as every route answers it, with its trust setup.
func (t *stubFedTarget) row() map[string]any {
	grants := []stubFedGrant{}
	grants = append(grants, t.Grants...)
	row := map[string]any{
		"target_ref": t.Ref, "name": t.Name, "project": t.Project, "label": t.Label, "description": t.Description,
		"cloud": t.Cloud, "audience": stubFedAudience(t), "grants": grants, "status": t.Status,
	}
	prefix := "fed:" + t.Ref + ":"
	setup := map[string]any{"issuer": stubIssuer, "jwks_uri": stubIssuer + "/.well-known/jwks.json", "audience": stubFedAudience(t),
		"subject": prefix + "<product>:<kind|workload>"}
	switch t.Cloud {
	case "gcp":
		row["gcp"] = map[string]any{"workload_identity_provider": t.WIP, "service_account": t.ServiceAccount, "scopes": t.Scopes}
		pool, _, _ := strings.Cut(t.WIP, "/providers/")
		setup["gcp"] = map[string]any{
			"issuer_uri":          stubIssuer,
			"attribute_condition": fmt.Sprintf("assertion.tenant == %q && assertion.target == %q", t.Tenant, t.Ref),
			"principal":           "principal://iam.googleapis.com/" + pool + "/subject/" + prefix + "<product>:<kind|workload>",
			"principal_set":       "principalSet://iam.googleapis.com/" + pool + "/attribute.target/" + t.Ref,
		}
	case "aws":
		row["aws"] = map[string]any{"role_arn": t.RoleARN, "region": t.Region, "session_policy": t.SessionPolicy, "max_duration": t.MaxDuration}
		setup["aws"] = map[string]any{"provider_url": stubIssuer}
	case "azure":
		row["azure"] = map[string]any{"tenant_id": t.AzureTenantID, "client_id": t.ClientID, "scope": t.Scope, "hosts": t.Hosts}
		setup["azure"] = map[string]any{"issuer": stubIssuer}
	case "oidc":
		row["oidc"] = map[string]any{"token_url": t.TokenURL, "grant": t.Grant, "hosts": t.Hosts}
		setup["oidc"] = map[string]any{"issuer": stubIssuer}
	}
	row["setup"] = setup
	return row
}

// set applies a PUT's body the way SetFederationTarget does; msg is the
// refusal.
func (t *stubFedTarget) set(b stubFedBody) (code int, msg string) {
	if t.Status != "" && t.Cloud != b.Cloud {
		return http.StatusBadRequest, "a federation target's cloud never changes: delete it and set another"
	}
	next := *t
	next.Cloud, next.Audience = b.Cloud, strings.TrimSpace(b.Audience)
	next.WIP, next.ServiceAccount, next.Scopes = "", "", nil
	next.RoleARN, next.Region, next.SessionPolicy, next.MaxDuration = "", "", "", 0
	next.AzureTenantID, next.ClientID, next.Scope = "", "", ""
	next.TokenURL, next.Grant, next.Hosts = "", "", []string{}
	blocks := 0
	for _, set := range []bool{b.GCP != nil, b.AWS != nil, b.Azure != nil, b.OIDC != nil} {
		if set {
			blocks++
		}
	}
	if blocks > 1 {
		return http.StatusBadRequest, fmt.Sprintf("a %s target names only its own cloud's fields", b.Cloud)
	}
	switch b.Cloud {
	case "gcp":
		if b.GCP == nil || !stubFedWIPRe.MatchString(b.GCP.WorkloadIdentityProvider) {
			return http.StatusBadRequest, "workload_identity_provider is projects/<number>/locations/global/workloadIdentityPools/<pool>/providers/<provider>"
		}
		next.WIP, next.ServiceAccount = b.GCP.WorkloadIdentityProvider, strings.ToLower(strings.TrimSpace(b.GCP.ServiceAccount))
		if next.ServiceAccount != "" && !strings.HasSuffix(next.ServiceAccount, ".iam.gserviceaccount.com") {
			return http.StatusBadRequest, "service_account is a service account's email (<name>@<project>.iam.gserviceaccount.com)"
		}
		for _, s := range b.GCP.Scopes {
			if !strings.HasPrefix(strings.TrimSpace(s), "https://www.googleapis.com/auth/") {
				return http.StatusBadRequest, fmt.Sprintf("scope %q is not a Google OAuth scope", s)
			}
		}
		if next.Scopes = stubFedSorted(b.GCP.Scopes); len(next.Scopes) == 0 {
			next.Scopes = []string{"https://www.googleapis.com/auth/cloud-platform"}
		}
	case "aws":
		if b.AWS == nil || !strings.HasPrefix(b.AWS.RoleARN, "arn:aws") || b.AWS.Region == "" {
			return http.StatusBadRequest, "role_arn is an IAM role's ARN (arn:aws:iam::<account>:role/<name>)"
		}
		next.RoleARN, next.Region, next.SessionPolicy, next.MaxDuration = b.AWS.RoleARN, b.AWS.Region, strings.TrimSpace(b.AWS.SessionPolicy), b.AWS.MaxDuration
		if next.SessionPolicy != "" && !json.Valid([]byte(next.SessionPolicy)) {
			return http.StatusBadRequest, "session_policy is a JSON policy document"
		}
		if next.MaxDuration == 0 {
			next.MaxDuration = 3600
		}
		if next.MaxDuration < 900 || next.MaxDuration > 3600 {
			return http.StatusBadRequest, "max_duration_seconds is between 900 and 3600"
		}
	case "azure":
		if b.Azure == nil || !stubFedGUIDRe.MatchString(b.Azure.TenantID) || !stubFedGUIDRe.MatchString(b.Azure.ClientID) {
			return http.StatusBadRequest, "azure_tenant_id and client_id are GUIDs"
		}
		if strings.TrimSpace(b.Azure.Scope) == "" {
			return http.StatusBadRequest, "scope is the resource's scope (https://management.azure.com/.default)"
		}
		next.AzureTenantID, next.ClientID = strings.ToLower(b.Azure.TenantID), strings.ToLower(b.Azure.ClientID)
		next.Scope, next.Hosts = strings.TrimSpace(b.Azure.Scope), stubFedSorted(b.Azure.Hosts)
		if len(next.Hosts) == 0 && !strings.HasPrefix(next.Scope, "https://") {
			return http.StatusBadRequest, fmt.Sprintf("scope %q names no resource host: name the hosts the token is written on", next.Scope)
		}
	case "oidc":
		if b.OIDC == nil {
			return http.StatusBadRequest, "an oidc target names the hosts its token is written on"
		}
		next.TokenURL, next.Grant, next.Hosts = strings.TrimSpace(b.OIDC.TokenURL), strings.TrimSpace(b.OIDC.Grant), stubFedSorted(b.OIDC.Hosts)
		switch {
		case next.TokenURL != "" && !strings.HasPrefix(next.TokenURL, "https://"):
			return http.StatusBadRequest, "token_url is an https URL with no user or fragment"
		case next.TokenURL != "" && next.Grant == "":
			next.Grant = "urn:ietf:params:oauth:grant-type:token-exchange"
		case next.TokenURL == "" && next.Grant != "":
			return http.StatusBadRequest, "a grant is a token_url's: a direct oidc target names neither"
		}
		if next.Audience == "" {
			return http.StatusBadRequest, "an oidc target names its audience"
		}
		if len(next.Hosts) == 0 {
			return http.StatusBadRequest, "an oidc target names the hosts its token is written on"
		}
	default:
		return http.StatusBadRequest, "cloud is gcp, aws, azure or oidc"
	}
	if b.Label != nil {
		next.Label = strings.TrimSpace(*b.Label)
	}
	if next.Label == "" {
		next.Label = next.Name
	}
	if b.Description != nil {
		next.Description = strings.TrimSpace(*b.Description)
	}
	if b.Status != nil {
		next.Status = *b.Status
	}
	switch next.Status {
	case "":
		next.Status = "active"
	case "active", "disabled":
	default:
		return http.StatusBadRequest, "status is active or disabled"
	}
	*t = next
	return 0, ""
}

// federationRoutes mounts the routes under the org's prefix.
func (s *stubState) federationRoutes(mux *http.ServeMux, org string, authed func(http.HandlerFunc) http.HandlerFunc) {
	const one = "/tenants/{tenant}/federation-targets/{name}"
	prefix := "/org/" + org
	oops := func(w http.ResponseWriter, code int, msg string) {
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
	// the route's target: its key, and the target when it was ever set
	at := func(w http.ResponseWriter, r *http.Request) (key string, t *stubFedTarget, ok bool) {
		tenant, _ := stubSlug(r.PathValue("tenant"))
		if tn, exists := s.tenants[tenant]; !exists || tn.Status != "active" {
			oops(w, http.StatusNotFound, "unknown tenant")
			return "", nil, false
		}
		name := strings.ToLower(strings.TrimSpace(r.PathValue("name")))
		if !stubFedNameRe.MatchString(name) {
			oops(w, http.StatusBadRequest, "name is lower-case letters, digits and dashes (at most 63)")
			return "", nil, false
		}
		key = tenant + "|" + r.URL.Query().Get("project") + "|" + name
		return key, s.fedTargets[key], true
	}
	live := func(w http.ResponseWriter, t *stubFedTarget) bool {
		if t == nil || t.Status == "deleted" {
			oops(w, http.StatusNotFound, "no such federation target")
			return false
		}
		return true
	}

	mux.HandleFunc("PUT "+prefix+one, authed(func(w http.ResponseWriter, r *http.Request) {
		key, t, ok := at(w, r)
		if !ok {
			return
		}
		var b stubFedBody
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&b); err != nil {
			oops(w, http.StatusBadRequest, "the body is {cloud, audience?, gcp|aws|azure|oidc, label?, description?, status?}")
			return
		}
		created := t == nil
		if created {
			tenant, _ := stubSlug(r.PathValue("tenant"))
			project := r.URL.Query().Get("project")
			name := strings.ToLower(strings.TrimSpace(r.PathValue("name")))
			t = &stubFedTarget{Tenant: tenant, Project: project, Name: name,
				Ref: uuid.NewSHA1(uuid.NameSpaceURL, []byte("latchkey:federation-target:"+org+"/"+tenant+"|"+project+"|"+name)).String()}
		}
		if t.Status == "deleted" {
			oops(w, http.StatusConflict, fmt.Sprintf("federation target %q was deleted; its name stays taken", t.Name))
			return
		}
		if code, msg := t.set(b); code != 0 {
			oops(w, code, msg)
			return
		}
		s.fedTargets[key] = t
		if created {
			w.WriteHeader(http.StatusCreated)
		}
		json.NewEncoder(w).Encode(t.row())
	}))
	mux.HandleFunc("GET "+prefix+one, authed(func(w http.ResponseWriter, r *http.Request) {
		if _, t, ok := at(w, r); ok && live(w, t) {
			json.NewEncoder(w).Encode(t.row())
		}
	}))
	mux.HandleFunc("DELETE "+prefix+one, authed(func(w http.ResponseWriter, r *http.Request) {
		_, t, ok := at(w, r)
		if !ok {
			return
		}
		if t == nil {
			oops(w, http.StatusNotFound, "no such federation target")
			return
		}
		// final, and converging: the name stays taken
		t.Status, t.Grants = "deleted", nil
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("PUT "+prefix+one+"/grants/{product}", authed(func(w http.ResponseWriter, r *http.Request) {
		_, t, ok := at(w, r)
		if !ok {
			return
		}
		var b struct {
			Purposes     []string `json:"purposes"`
			Kinds        []string `json:"kinds"`
			Environments []string `json:"environments"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&b); err != nil {
			oops(w, http.StatusBadRequest, "the body is {purposes, kinds?, environments?}")
			return
		}
		if !live(w, t) {
			return
		}
		product := strings.ToLower(strings.TrimSpace(r.PathValue("product")))
		if !stubFedGrantRe.MatchString(product) {
			oops(w, http.StatusBadRequest, fmt.Sprintf("product %q is not a product name", product))
			return
		}
		g := stubFedGrant{Product: product, Purposes: stubFedSorted(b.Purposes), Kinds: stubFedSorted(b.Kinds),
			Environments: stubFedSorted(b.Environments), GrantedBy: "client:stub"}
		if len(g.Purposes) == 0 {
			oops(w, http.StatusBadRequest, "a grant names at least one purpose")
			return
		}
		t.Grants = slices.DeleteFunc(t.Grants, func(old stubFedGrant) bool { return old.Product == product })
		t.Grants = append(t.Grants, g)
		sort.Slice(t.Grants, func(i, j int) bool { return t.Grants[i].Product < t.Grants[j].Product })
		json.NewEncoder(w).Encode(t.row())
	}))
	mux.HandleFunc("DELETE "+prefix+one+"/grants/{product}", authed(func(w http.ResponseWriter, r *http.Request) {
		_, t, ok := at(w, r)
		if !ok || !live(w, t) {
			return
		}
		product := strings.ToLower(strings.TrimSpace(r.PathValue("product")))
		t.Grants = slices.DeleteFunc(t.Grants, func(old stubFedGrant) bool { return old.Product == product })
		json.NewEncoder(w).Encode(t.row())
	}))
}
