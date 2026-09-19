// quarantine_config_test.go quarantine / probation 配置段测试：
// 默认值 / 文件覆盖 / 空值回落 / 非法值 fail fast / 非正阈值回落。
package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestQuarantineDefaultConfig 键缺席 → 隔离号池与观察池默认开启且参数为设计值。
func TestQuarantineDefaultConfig(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.Quarantine.Enabled {
		t.Errorf("quarantine.enabled want true (default)")
	}
	if c.Quarantine.Threshold != 1 {
		t.Errorf("quarantine.threshold=%d want 1（内容是恶性错误：一次即隔离）", c.Quarantine.Threshold)
	}
	if c.QuarantineProbeDelayDur != time.Minute {
		t.Errorf("quarantine.probe_delay=%v want 1m", c.QuarantineProbeDelayDur)
	}
	if c.QuarantineSilenceDur != time.Hour {
		t.Errorf("quarantine.silence=%v want 1h", c.QuarantineSilenceDur)
	}
	if c.QuarantineSilenceMaxDur != 24*time.Hour {
		t.Errorf("quarantine.silence_max=%v want 24h", c.QuarantineSilenceMaxDur)
	}
	if c.QuarantineProbeIntervalDur != 5*time.Minute {
		t.Errorf("quarantine.probe_interval=%v want 5m", c.QuarantineProbeIntervalDur)
	}
	if !c.Probation.Enabled {
		t.Errorf("probation.enabled want true (default)")
	}
	if c.Probation.PromoteSuccesses != 3 {
		t.Errorf("probation.promote_successes=%d want 3", c.Probation.PromoteSuccesses)
	}
	if c.ProbationCanaryIntervalDur != 5*time.Minute {
		t.Errorf("probation.canary_interval=%v want 5m", c.ProbationCanaryIntervalDur)
	}
}

// TestQuarantineParsedFromFile 显式配置覆盖默认（含退回旧的「连续 N 次才隔离」形态）。
func TestQuarantineParsedFromFile(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(fp, []byte(`{
		"quarantine":{"enabled":false,"threshold":3,"probe_delay":"30s","silence":"2h","silence_max":"12h","probe_interval":"10m"},
		"probation":{"enabled":false,"promote_successes":5,"canary_interval":"15m"}
	}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Quarantine.Enabled || c.Quarantine.Threshold != 3 {
		t.Errorf("quarantine enabled=%v threshold=%d want false/3", c.Quarantine.Enabled, c.Quarantine.Threshold)
	}
	if c.QuarantineProbeDelayDur != 30*time.Second || c.QuarantineSilenceDur != 2*time.Hour ||
		c.QuarantineSilenceMaxDur != 12*time.Hour || c.QuarantineProbeIntervalDur != 10*time.Minute {
		t.Errorf("quarantine durations not parsed: %v/%v/%v/%v",
			c.QuarantineProbeDelayDur, c.QuarantineSilenceDur, c.QuarantineSilenceMaxDur, c.QuarantineProbeIntervalDur)
	}
	if c.Probation.Enabled || c.Probation.PromoteSuccesses != 5 || c.ProbationCanaryIntervalDur != 15*time.Minute {
		t.Errorf("probation not parsed: enabled=%v promote=%d canary=%v",
			c.Probation.Enabled, c.Probation.PromoteSuccesses, c.ProbationCanaryIntervalDur)
	}
}

// TestQuarantineEmptyAndNonPositiveFallBack 空串与 "0"（非正阈值）回落设计默认，
// 不误解为「关停」——关停由 enabled=false 表达（唯一开关）。
func TestQuarantineEmptyAndNonPositiveFallBack(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(fp, []byte(`{
		"quarantine":{"threshold":0,"probe_delay":"","silence":"","silence_max":"","probe_interval":""},
		"probation":{"promote_successes":0,"canary_interval":""}
	}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Quarantine.Threshold != 1 || c.QuarantineProbeDelayDur != time.Minute ||
		c.QuarantineSilenceDur != time.Hour || c.QuarantineSilenceMaxDur != 24*time.Hour ||
		c.QuarantineProbeIntervalDur != 5*time.Minute {
		t.Errorf("empty/non-positive quarantine fields should fall back to defaults: %+v", c.Quarantine)
	}
	if c.Probation.PromoteSuccesses != 3 || c.ProbationCanaryIntervalDur != 5*time.Minute {
		t.Errorf("empty/non-positive probation fields should fall back to defaults: %+v", c.Probation)
	}
}

// TestBadQuarantineDurations 非法时长 fail fast（不静默回落，风格同 soft_rate_max）。
func TestBadQuarantineDurations(t *testing.T) {
	cases := []string{
		`{"quarantine":{"probe_delay":"oops"}}`,
		`{"quarantine":{"silence":"oops"}}`,
		`{"quarantine":{"silence_max":"oops"}}`,
		`{"quarantine":{"probe_interval":"oops"}}`,
		`{"probation":{"canary_interval":"oops"}}`,
	}
	for i, body := range cases {
		fp := filepath.Join(t.TempDir(), "c.json")
		os.WriteFile(fp, []byte(body), 0o600)
		if _, err := Load(fp); err == nil {
			t.Errorf("case %d (%s): want error for bad duration", i, body)
		}
	}
}
