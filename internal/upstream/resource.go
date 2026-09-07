package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"workbuddy2api/internal/auth"
)

// ResourcePackage mirrors one credit package from the billing
// get-user-resource endpoint (data.Response.Data.Accounts[]).
type ResourcePackage struct {
	PackageName         string `json:"packageName"`
	CycleCapacitySize   int64  `json:"total"`
	CycleCapacityUsed   int64  `json:"used"`
	CycleCapacityRemain int64  `json:"remaining"`
	CycleEndTime        string `json:"resetAt"`
	Recurring           bool   `json:"recurring"`
}

// ResourcePackages fetches the full credit-package list for an account.
// Same billing endpoint as UserResource but returns every package
// (name/used/total/remaining/reset) so quota dashboards can render one row
// per package — including Complimentary bag entries. NO ProductCode/Status
// filter is sent: filtering hides gift/bonus packages on some accounts.
// Works for both realms: billingBase resolves CN (codebuddy.cn) vs Global
// (workbuddy.ai) from the auth domain.
func (c *Client) ResourcePackages(a *auth.Auth) ([]ResourcePackage, error) {
	url := c.billingBase(a) + "/v2/billing/meter/get-user-resource"
	body := map[string]any{
		"PageNumber": 1,
		"PageSize":   200,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	BillingHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CycleStartTime      string `json:"CycleStartTime"`
					CycleEndTime        string `json:"CycleEndTime"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("resource parse: %w", err)
	}
	packages := make([]ResourcePackage, 0, len(resp.Response.Data.Accounts))
	for _, acct := range resp.Response.Data.Accounts {
		total, used, remain := acct.CapacitySize, acct.CapacityUsed, acct.CapacityRemain
		cycle := acct.CycleCapacitySize > 0
		if cycle {
			total, used, remain = acct.CycleCapacitySize, acct.CycleCapacityUsed, acct.CycleCapacityRemain
		}
		if remain < 0 {
			remain = 0
		}
		name := acct.PackageName
		if name == "" {
			name = "Credit Package"
		}
		packages = append(packages, ResourcePackage{
			PackageName:         name,
			CycleCapacitySize:   total,
			CycleCapacityUsed:   used,
			CycleCapacityRemain: remain,
			CycleEndTime:        acct.CycleEndTime,
			Recurring:           cycle,
		})
	}
	return packages, nil
}
