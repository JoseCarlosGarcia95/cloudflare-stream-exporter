package main

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/cloudflare/cloudflare-go"
	"github.com/machinebox/graphql"
	"github.com/namsral/flag"
	"github.com/nelkinda/health-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
)

var (
	cfGraphQLEndpoint = "https://api.cloudflare.com/client/v4/graphql/"
)

var (
	cfgListen         = ":8080"
	cfgCfAPIToken     = ""
	cfgMetricsPath    = "/metrics"
	cfIncludeAccounts = ""
)

type cfResponseStreamingAnalytics struct {
	Viewer struct {
		Accounts []cfResponseStreamingAnalyticsResp `json:"accounts"`
	} `json:"viewer"`
}

type cfResponseStreamingAnalyticsResp struct {
	AccountStreamMinutesViewedAdaptiveGroupsSum []struct {
		Sum struct {
			MinutesViewed uint64 `json:"minutesViewed"`
		} `json:"sum"`
		Dimensions struct {
			Ts time.Time `json:"ts"`
		} `json:"dimensions"`
	} `json:"streamMinutesViewedAdaptiveGroups"`
}

var (
	// Requests
	cfStreamingMinutesViewed = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cloudflare_streaming_minutes_viewed",
		Help: "Number of minutes viewed by a user",
	}, []string{"account"},
	)
)

func fetchAccounts() []cloudflare.Account {
	var api *cloudflare.API
	var err error
	if len(cfgCfAPIToken) > 0 {
		api, err = cloudflare.NewWithAPIToken(cfgCfAPIToken)
	}
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	a, _, err := api.Accounts(ctx, cloudflare.AccountsListParams{})
	if err != nil {
		log.Fatal(err)
	}

	return a
}

func fetchStreamingTotals(accountID string) (*cfResponseStreamingAnalytics, error) {
	now := time.Now()
	now30mAgo := now.Add(-30 * time.Minute)

	request := graphql.NewRequest(`
	query ($accountID: String!, $mintime: Time!, $maxtime: Time!) {
		viewer {
			accounts(filter: {accountTag: $accountID} ) {
				streamMinutesViewedAdaptiveGroups(limit: 1000, orderBy: [sum_minutesViewed_DESC], filter: { datetime_geq: $mintime, datetime_lt: $maxtime}) {
					sum {
						minutesViewed
					}

					dimensions {
						ts: datetimeFiveMinutes
					}
				}
			}
		}
	}
`)
	if len(cfgCfAPIToken) > 0 {
		request.Header.Set("Authorization", "Bearer "+cfgCfAPIToken)
	}
	request.Var("maxtime", now)
	request.Var("mintime", now30mAgo)
	request.Var("accountID", accountID)

	ctx := context.Background()
	graphqlClient := graphql.NewClient(cfGraphQLEndpoint)
	var resp cfResponseStreamingAnalytics
	if err := graphqlClient.Run(ctx, request, &resp); err != nil {
		log.Error(err)
		return nil, err
	}

	return &resp, nil
}

func fetchStreamingAnalytics(account cloudflare.Account) {
	r, err := fetchStreamingTotals(account.ID)
	if err != nil {
		log.Error(err)
		return
	}

	for _, a := range r.Viewer.Accounts {
		sum := 0

		for _, b := range a.AccountStreamMinutesViewedAdaptiveGroupsSum {
			sum += int(b.Sum.MinutesViewed)
		}

		cfStreamingMinutesViewed.With(prometheus.Labels{"account": account.Name}).Set(float64(sum) / float64(len(a.AccountStreamMinutesViewedAdaptiveGroupsSum)))
	}
}

func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

func fetchMetrics() {
	accounts := fetchAccounts()

	accountsToHandle := strings.Split(cfIncludeAccounts, ",")

	for _, a := range accounts {
		if len(accountsToHandle) > 0 {
			if !contains(accountsToHandle, a.ID) {
				continue
			}
		}

		log.Printf("Fetching streaming analytics for %s", a.Name)
		fetchStreamingAnalytics(a)
	}
}

func main() {
	flag.StringVar(&cfgListen, "listen", cfgListen, "listen on addr:port ( default :8080), omit addr to listen on all interfaces")
	flag.StringVar(&cfgCfAPIToken, "cf_api_token", cfgCfAPIToken, "cloudflare api token (preferred)")
	flag.StringVar(&cfIncludeAccounts, "include_accounts", cfIncludeAccounts, "comma-separated list of accounts to include")
	flag.Parse()
	if !(len(cfgCfAPIToken) > 0) {
		log.Fatal("Please provide CF_API_KEY+CF_API_EMAIL or CF_API_TOKEN")
	}
	customFormatter := new(log.TextFormatter)
	customFormatter.TimestampFormat = "2006-01-02 15:04:05"
	log.SetFormatter(customFormatter)
	customFormatter.FullTimestamp = true

	go func() {
		for ; true; <-time.NewTicker(60 * time.Second).C {
			fetchMetrics()
		}
	}()

	//This section will start the HTTP server and expose
	//any metrics on the /metrics endpoint.
	if !strings.HasPrefix(cfgMetricsPath, "/") {
		cfgMetricsPath = "/" + cfgMetricsPath
	}
	http.Handle(cfgMetricsPath, promhttp.Handler())
	h := health.New(health.Health{})
	http.HandleFunc("/health", h.Handler)
	log.Info("Beginning to serve on port", cfgListen, ", metrics path ", cfgMetricsPath)
	log.Fatal(http.ListenAndServe(cfgListen, nil))
}
