package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseNested(t *testing.T) {
	raw := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1753600000,"domain":""},"account":{"uid":"u1","enterpriseId":"e1","nickname":"n1"}}`)
	sa, err := Parse(raw)
	if err != nil {
		t.Fatalf("nested parse err: %v", err)
	}
	if sa.AccessToken != "at" || sa.RefreshToken != "rt" || sa.ExpiresAt != 1753600000 {
		t.Errorf("tokens: %+v", sa)
	}
	if sa.UID != "u1" || sa.EnterpriseID != "e1" || sa.Nickname != "n1" {
		t.Errorf("account: %+v", sa)
	}
}

func TestParseFlat(t *testing.T) {
	raw := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1753600000,"uid":"u2","nickname":"n2"}`)
	sa, err := Parse(raw)
	if err != nil || sa.UID != "u2" || sa.AccessToken != "at" {
		t.Fatalf("flat: %+v %v", sa, err)
	}
}

func TestParseMissingToken(t *testing.T) {
	if _, err := Parse([]byte(`{"uid":"u3"}`)); err == nil {
		t.Fatal("want error for missing accessToken")
	}
}

func TestSaveAtomicRoundtrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000,
		UID: "u1", EnterpriseID: "e1", Nickname: "n1", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(fp + ".tmp"); !os.IsNotExist(err) {
		t.Error("tmp file should not remain")
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.AccessToken != "at" || b.UID != "u1" || b.EnterpriseID != "e1" {
		t.Errorf("roundtrip: %+v", b)
	}
}

// TestLoadDirLoadsAllValid 不再按 region 过滤：所有可解析的 auth 文件都被加载，
// 解析失败的文件静默跳过。
func TestLoadDirLoadsAllValid(t *testing.T) {
	dir := t.TempDir()
	cn := `{"auth":{"accessToken":"at1","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"cn1"}}`
	other := `{"auth":{"accessToken":"at2","refreshToken":"r","expiresAt":1,"domain":"example.com"},"account":{"uid":"u2"}}`
	bad := `not json`
	os.WriteFile(filepath.Join(dir, "workbuddy-cn1.json"), []byte(cn), 0o600)
	os.WriteFile(filepath.Join(dir, "workbuddy-u2.json"), []byte(other), 0o600)
	os.WriteFile(filepath.Join(dir, "workbuddy-bad.json"), []byte(bad), 0o600)

	list, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 valid accounts, got %+v", list)
	}
	for _, a := range list {
		if a.FilePath == "" {
			t.Error("FilePath not set")
		}
	}
}

func TestNeedsRefresh(t *testing.T) {
	a := &Auth{ExpiresAt: 0}
	if !a.NeedsRefresh(0) {
		t.Error("zero expiry should need refresh")
	}
	a.ExpiresAt = 9999999999
	if a.NeedsRefresh(0) {
		t.Error("far future should not need refresh")
	}
}

// TestParseMachineId 嵌套形与扁平形 auth 文件的顶层 machine_id 键均被解析。
func TestParseMachineId(t *testing.T) {
	nested := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,"domain":""},"account":{"uid":"u1"},"machine_id":"nested-mid"}`)
	sa, err := Parse(nested)
	if err != nil {
		t.Fatalf("nested parse: %v", err)
	}
	if sa.MachineId != "nested-mid" {
		t.Errorf("nested MachineId = %q want %q", sa.MachineId, "nested-mid")
	}

	flat := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1,"uid":"u2","machine_id":"flat-mid"}`)
	fa, err := Parse(flat)
	if err != nil {
		t.Fatalf("flat parse: %v", err)
	}
	if fa.MachineId != "flat-mid" {
		t.Errorf("flat MachineId = %q want %q", fa.MachineId, "flat-mid")
	}
}

// TestEnsureMachineId 显式配置优先，缺省派生幂等（同一账号稳定、不同账号不同）。
func TestEnsureMachineId(t *testing.T) {
	// 显式配置原样保留。
	explicit := &Auth{UID: "u1", MachineId: "custom-mid"}
	if got := explicit.EnsureMachineId(); got != "custom-mid" {
		t.Errorf("EnsureMachineId explicit = %q want %q", got, "custom-mid")
	}
	// 缺省派生：幂等且非空。
	a := &Auth{UID: "acct-1"}
	m1 := a.EnsureMachineId()
	m2 := a.EnsureMachineId()
	if m1 != m2 || m1 == "" {
		t.Errorf("derived machineId not stable: %q vs %q", m1, m2)
	}
	if len(m1) != 48 {
		t.Errorf("derived length = %d want 48 hex", len(m1))
	}
	// 不同账号派生不同。
	b := &Auth{UID: "acct-2"}
	if b.EnsureMachineId() == m1 {
		t.Errorf("different uid should derive different machineId")
	}
}

// TestSaveAtomicPreservesMachineId SaveAtomic 写回后显式 machine_id 被保留。
func TestSaveAtomicPreservesMachineId(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-mid.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1,
		UID: "u1", MachineId: "persisted-mid", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.MachineId != "persisted-mid" {
		t.Errorf("roundtrip MachineId = %q want %q", b.MachineId, "persisted-mid")
	}
}

// TestParseDeviceToken 嵌套形与扁平形 auth 文件的顶层 device_token 键均被解析。
func TestParseDeviceToken(t *testing.T) {
	nested := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,"domain":""},"account":{"uid":"u1"},"device_token":"dev-tok-nested"}`)
	sa, err := Parse(nested)
	if err != nil {
		t.Fatalf("nested parse: %v", err)
	}
	if sa.DeviceToken != "dev-tok-nested" {
		t.Errorf("nested DeviceToken = %q want %q", sa.DeviceToken, "dev-tok-nested")
	}

	flat := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1,"uid":"u2","device_token":"dev-tok-flat"}`)
	fa, err := Parse(flat)
	if err != nil {
		t.Fatalf("flat parse: %v", err)
	}
	if fa.DeviceToken != "dev-tok-flat" {
		t.Errorf("flat DeviceToken = %q want %q", fa.DeviceToken, "dev-tok-flat")
	}
}

// TestSaveAtomicPreservesDeviceToken SaveAtomic 写回后顶层 device_token 被保留并重新解析回来。
func TestSaveAtomicPreservesDeviceToken(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-dt.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1,
		UID: "u1", DeviceToken: "persisted-tok", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.DeviceToken != "persisted-tok" {
		t.Errorf("roundtrip DeviceToken = %q want %q", b.DeviceToken, "persisted-tok")
	}
}
