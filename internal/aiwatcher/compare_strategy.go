package aiwatcher

import "strings"

type compareExtractor func(map[string]any) any
type kindMatcher func(string) bool

type compareStrategy struct {
	name      string
	matcher   kindMatcher
	extractor compareExtractor
}

// Ordered matchers for drift extraction (see README "Current field strategies").
// Unlisted kinds use strategyForKind fallback: full spec.
var compareStrategies = []compareStrategy{
	{name: "data", matcher: matchKinds("configmap"), extractor: extractDataField},
	{name: "data", matcher: matchKinds("secret"), extractor: extractDataField},
	{name: "networkPolicySpec", matcher: matchKinds("networkpolicy"), extractor: extractNetworkPolicySpec},
	{name: "rules", matcher: matchKinds("role", "clusterrole"), extractor: extractRulesField},
	{name: "webhooks", matcher: matchKinds("mutatingwebhookconfiguration", "validatingwebhookconfiguration"), extractor: extractWebhooksField},
	{name: "serviceAccountFields", matcher: matchKinds("serviceaccount"), extractor: normalizeServiceAccountFields},
	{name: "resourceQuotaFields", matcher: matchKinds("resourcequota"), extractor: normalizeResourceQuotaFields},
	{name: "limitRangeFields", matcher: matchKinds("limitrange"), extractor: normalizeLimitRangeFields},
}

func strategyForKind(kind string) compareStrategy {
	kind = strings.ToLower(kind)
	for _, s := range compareStrategies {
		if s.matcher(kind) {
			return s
		}
	}
	return compareStrategy{
		name:      "spec",
		matcher:   func(string) bool { return true },
		extractor: extractSpecField,
	}
}

func matchKinds(kinds ...string) kindMatcher {
	set := map[string]struct{}{}
	for _, k := range kinds {
		set[strings.ToLower(k)] = struct{}{}
	}
	return func(kind string) bool {
		_, ok := set[strings.ToLower(kind)]
		return ok
	}
}
