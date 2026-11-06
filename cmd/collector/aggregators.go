package main

import (
	"net/http"

	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator/enso"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator/fly"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator/kyberswap"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator/lifi"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator/nordstern"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator/sushi"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator/velora"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/aggregator/zerox"
	"github.com/IceCreamSwapCom/dex-aggregator-competitiveness/internal/upstream"
)

// keyEnv names the environment variable with the API key of each aggregator
// that needs one: there is no keyless tier (0x, Enso), or the keyless one
// allows far fewer quotes than the benchmark asks for and charges a fee
// (LI.FI). A configured aggregator without its key stops the start: every
// answer would be a refusal, counted against the aggregator.
var keyEnv = map[string]string{
	"zerox": "ZEROX_API_KEY",
	"lifi":  "LIFI_API_KEY",
	"enso":  "ENSO_API_KEY",
}

// nordsternReferer identifies the benchmark to Nordstern, as the integrator id
// does to KyberSwap and the keys to 0x, LI.FI and Enso: each aggregator is
// asked the way that gets its best offer (METHODOLOGY.md, 1).
const nordsternReferer = "https://aggregator-benchmark.nordstern.finance"

// aggregators are all the adapters, by id. Each one talks to the public API
// every integrator gets; key returns an API key by its environment variable.
// Keys go only into request headers, which are never logged.
func aggregators(hc *http.Client, integratorID string, key func(string) string) map[string]aggregator.Aggregator {
	api := func(base string, header http.Header) *upstream.Client { return upstream.New(hc, base, header) }
	all := map[string]aggregator.Aggregator{}
	for _, a := range []aggregator.Aggregator{
		nordstern.New(api("https://api.nordstern.finance", http.Header{"Referer": {nordsternReferer}})),
		kyberswap.New(api("https://aggregator-api.kyberswap.com", http.Header{"X-Client-Id": {integratorID}})),
		velora.New(api("https://api.velora.xyz", nil)),
		sushi.New(api("https://api.sushi.com", nil)),
		fly.New(api("https://api.fly.trade", nil)),
		zerox.New(api("https://api.0x.org", http.Header{"0x-Api-Key": {key(keyEnv["zerox"])}, "0x-Version": {"v2"}})),
		lifi.New(api("https://li.quest", http.Header{"X-Lifi-Api-Key": {key(keyEnv["lifi"])}})),
		enso.New(api("https://api.enso.build", http.Header{"Authorization": {"Bearer " + key(keyEnv["enso"])}})),
	} {
		all[a.ID()] = a
	}
	return all
}
