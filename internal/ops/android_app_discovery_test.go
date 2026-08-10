package ops

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func appQueryField(value string) string {
	if value == "" {
		return "-"
	}
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func validAppQueryFixture() string {
	return strings.Join([]string{
		"V\t1",
		strings.Join([]string{"A", appQueryField("bin.mt.plus.canary"), appQueryField("MT管理器"), "0", "1", "1", "0", "0", appQueryField("/data/app/mt/base.apk")}, "\t"),
		strings.Join([]string{"L", appQueryField("bin.mt.plus.canary"), appQueryField("launcher"), appQueryField("bin.mt.plus.canary/.MainLightIcon"), appQueryField("MT管理器"), "1", "1", "-", "0", "0", "0", "0"}, "\t"),
		"T\t1\t1\t0",
	}, "\n") + "\n"
}

func TestParseAppQueryOutput(t *testing.T) {
	apps, total, truncated, err := parseAppQueryOutput(validAppQueryFixture())
	if err != nil {
		t.Fatal(err)
	}
	if truncated || total != 1 || len(apps) != 1 {
		t.Fatalf("unexpected result: total=%d truncated=%v apps=%+v", total, truncated, apps)
	}
	app := apps[0]
	if app.Package != "bin.mt.plus.canary" || app.Label != "MT管理器" || !app.Installed || !app.Enabled || len(app.Activities) != 1 {
		t.Fatalf("application record was not preserved: %+v", app)
	}
	activity := app.Activities[0]
	if activity.Component != "bin.mt.plus.canary/bin.mt.plus.canary.MainLightIcon" || !activity.Enabled || !activity.Exported {
		t.Fatalf("launcher record was not normalized: %+v", activity)
	}
}

func TestParseAppQueryOutputRejectsUntrustedRecords(t *testing.T) {
	valid := validAppQueryFixture()
	application := strings.Split(valid, "\n")[1]
	launcher := strings.Split(valid, "\n")[2]
	for name, output := range map[string]string{
		"missing summary":         strings.TrimSuffix(valid, "T\t1\t1\t0\n"),
		"mismatched count":        strings.Replace(valid, "T\t1\t1\t0", "T\t2\t0\t1", 1),
		"duplicate application":   "V\t1\n" + application + "\n" + application + "\nT\t2\t2\t0\n",
		"duplicate launcher":      "V\t1\n" + application + "\n" + launcher + "\n" + launcher + "\nT\t1\t1\t0\n",
		"orphan launcher":         "V\t1\n" + launcher + "\nT\t0\t0\t0\n",
		"record after summary":    valid + application + "\n",
		"unknown record":          "V\t1\nX\tbad\nT\t0\t0\t0\n",
		"invalid boolean":         strings.Replace(valid, "\t0\t1\t1\t0\t0\t", "\t0\t2\t1\t0\t0\t", 1),
		"uninstalled application": strings.Replace(valid, "\t0\t1\t1\t0\t0\t", "\t0\t0\t1\t0\t0\t", 1),
		"cross-package component": strings.Replace(valid, appQueryField("bin.mt.plus.canary/.MainLightIcon"), appQueryField("com.example/.Main"), 1),
		"excessive match total":   strings.Replace(valid, "T\t1\t1\t0", "T\t4097\t1\t1", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := parseAppQueryOutput(output); err == nil {
				t.Fatalf("untrusted helper output was accepted:\n%s", output)
			}
		})
	}
}

func TestParseAppQueryOutputRejectsExcessiveLaunchers(t *testing.T) {
	lines := []string{
		"V\t1",
		strings.Join([]string{"A", appQueryField("com.example.app"), appQueryField("示例"), "0", "1", "1", "0", "0", appQueryField("/data/app/example/base.apk")}, "\t"),
	}
	for index := 0; index <= appQueryMaxLaunchers; index++ {
		component := "com.example.app/com.example.app.Main" + strconv.Itoa(index)
		lines = append(lines, strings.Join([]string{"L", appQueryField("com.example.app"), appQueryField("launcher"), appQueryField(component), appQueryField("示例"), "1", "1", "-", "0", "0", "0", "0"}, "\t"))
	}
	lines = append(lines, "T\t1\t1\t0")
	if _, _, _, err := parseAppQueryOutput(strings.Join(lines, "\n") + "\n"); err == nil {
		t.Fatal("excessive PackageManager launchers were accepted")
	}
}

func TestAppQueryPaginationMetadata(t *testing.T) {
	for _, valid := range []struct {
		offset, limit, total, emitted int
		hasMore                       bool
	}{
		{0, 64, 384, 64, true},
		{320, 64, 384, 64, false},
		{384, 64, 384, 0, false},
		{500, 64, 384, 0, false},
	} {
		if err := validateAppQueryPage(valid.offset, valid.limit, valid.total, valid.emitted, valid.hasMore); err != nil {
			t.Errorf("valid page rejected: %+v: %v", valid, err)
		}
	}
	for _, invalid := range []struct {
		offset, limit, total, emitted int
		hasMore                       bool
	}{
		{0, 64, 384, 63, true},
		{0, 64, 384, 64, false},
		{384, 64, 384, 0, true},
	} {
		if err := validateAppQueryPage(invalid.offset, invalid.limit, invalid.total, invalid.emitted, invalid.hasMore); err == nil {
			t.Errorf("invalid page accepted: %+v", invalid)
		}
	}
}

func TestAppQueryNormalizationAndRanking(t *testing.T) {
	if got := normalizeAppQuery("  ＭＴ\t管理器  "); got != "mt 管理器" {
		t.Fatalf("NFKC normalization = %q", got)
	}
	candidates := []androidAppCandidate{
		{Package: "com.example.settings.service", Label: "设置服务"},
		{Package: "com.android.settings", Label: "设置"},
	}
	selected, ambiguous, err := selectAppCandidate("设置", candidates)
	if err != nil || len(ambiguous) != 0 || selected.Package != "com.android.settings" {
		t.Fatalf("exact label did not win: selected=%+v ambiguous=%+v err=%v", selected, ambiguous, err)
	}
	candidates = append(candidates, androidAppCandidate{Package: "com.vendor.settings", Label: "设置"})
	if _, ambiguous, err := selectAppCandidate("设置", candidates); err == nil || len(ambiguous) != 2 {
		t.Fatalf("duplicate exact labels were not rejected: ambiguous=%+v err=%v", ambiguous, err)
	}
}

func TestPackageManagerQueryUnicodeBoundary(t *testing.T) {
	if got, err := normalizePackageManagerQuery(strings.Repeat("中", 256)); err != nil || utf8.RuneCountInString(got) != 256 {
		t.Fatalf("256-code-point query = %d, %v", utf8.RuneCountInString(got), err)
	}
	for _, value := range []string{"", strings.Repeat("中", 257), "bad\x00query", string([]byte{0xff})} {
		if _, err := normalizePackageManagerQuery(value); err == nil {
			t.Errorf("invalid query was accepted: %q", value)
		}
	}
	page := strings.Replace(validAppQueryFixture(), "T\t1\t1\t0", "T\t6\t1\t0", 1)
	apps, total, hasMore, err := parseAppQueryOutput(page)
	if err != nil || total != 6 || hasMore || len(apps) != 1 {
		t.Fatalf("last page metadata = apps:%d total:%d hasMore:%v err:%v", len(apps), total, hasMore, err)
	}
}

func TestAndroidComponentNormalization(t *testing.T) {
	component, pkg, err := normalizeAndroidComponent("com.android.settings/.Settings")
	if err != nil || component != "com.android.settings/com.android.settings.Settings" || pkg != "com.android.settings" {
		t.Fatalf("valid short component = %q, %q, %v", component, pkg, err)
	}
	for _, value := range []string{"", "com.example", "com.example/", "../x/.Main", "com.example/.Main extra", "com.example/.Main\n--user", "com.example/.Mäin", "com.example/.Main/Other"} {
		if _, _, err := normalizeAndroidComponent(value); err == nil {
			t.Errorf("invalid component %q was accepted", value)
		}
	}
	output := "priority=0 preferredOrder=0\ncom.android.settings/.Settings\n"
	resolved, ok := parseResolvedComponent(output, "com.android.settings")
	if !ok || resolved != "com.android.settings/com.android.settings.Settings" {
		t.Fatalf("resolve output was not parsed: %q, %v", resolved, ok)
	}
	if _, ok := parseResolvedComponent("com.other/.Main\n", "com.android.settings"); ok {
		t.Fatal("cross-package resolver output was accepted")
	}
}

func TestAndroidLaunchSemanticErrors(t *testing.T) {
	for name, result := range map[string]Result{
		"exit zero status error": {Success: true, Stdout: "Status: error\nActivity: com.example/.Main"},
		"exit zero error text":   {Success: true, Stdout: "Error: Activity class com.example/.Missing does not exist."},
		"security exception":     {Success: true, Stderr: "java.lang.SecurityException: Permission Denial"},
		"process failure":        {Success: false, Error: "am exited 1"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := androidSemanticError(result); err == nil {
				t.Fatalf("semantic launch failure was accepted: %+v", result)
			}
		})
	}
	for name, result := range map[string]Result{
		"normal start":       {Success: true, Stdout: "Status: ok\nActivity: com.example/.Main"},
		"already foreground": {Success: true, Stdout: "Warning: Activity not started, intent has been delivered\nStatus: ok"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := androidWaitStartError(result); err != nil {
				t.Fatalf("successful launch was rejected: %v", err)
			}
		})
	}
	if err := androidWaitStartError(Result{Success: true, Stdout: "Starting: Intent { cmp=com.example/.Main }"}); err == nil {
		t.Fatal("wait launch without Status: ok was accepted")
	}
}

func TestAndroidEffectiveEnabledAndAliasFallback(t *testing.T) {
	for _, test := range []struct {
		manifest bool
		state    int
		want     bool
	}{
		{true, 0, true}, {false, 0, false}, {false, 1, true}, {true, 2, false}, {true, 3, false}, {true, 4, false},
	} {
		if got := androidEffectiveEnabled(test.manifest, test.state); got != test.want {
			t.Errorf("effective enabled(%v, %d) = %v, want %v", test.manifest, test.state, got, test.want)
		}
	}
	disabledAlias := androidLaunchCandidate{
		Kind: "launcher", Component: "com.example/com.example.Alias", Label: "示例",
		Enabled: true, Exported: true, State: 3,
		Target: "com.example/com.example.Main", TargetEnabled: true, TargetExported: true, TargetState: 0,
	}
	if rank := launchCandidateRank("示例", disabledAlias); rank != 850325 {
		t.Fatalf("disabled alias target rank = %d", rank)
	}
	if androidEffectiveEnabled(disabledAlias.Enabled, disabledAlias.State) {
		t.Fatal("disabled alias was treated as enabled")
	}
	component, strategy, ok := selectPackageManagerLaunchComponent("示例", []androidLaunchCandidate{disabledAlias})
	if !ok || component != disabledAlias.Target || strategy != "package_manager_disabled_alias_target" {
		t.Fatalf("disabled alias fallback = %q, %q, %v", component, strategy, ok)
	}
	if resolvedComponentUsable(androidAppCandidate{Activities: []androidLaunchCandidate{disabledAlias}}, disabledAlias.Component) {
		t.Fatal("resolver output reused a disabled alias")
	}
	if !resolvedComponentUsable(androidAppCandidate{Activities: []androidLaunchCandidate{disabledAlias}}, disabledAlias.Target) {
		t.Fatal("resolver output rejected an enabled alias target")
	}
}

func TestActivityLabelMatchOutranksPackageDefault(t *testing.T) {
	candidate := androidAppCandidate{
		Package: "com.example.camera", Label: "相机",
		Activities: []androidLaunchCandidate{
			{Kind: "launcher", Component: "com.example.camera/com.example.camera.Camera", Label: "相机", Enabled: true, Exported: true},
			{Kind: "launcher", Component: "com.example.camera/com.example.camera.Gallery", Label: "图库", Enabled: true, Exported: true},
		},
	}
	if !packageManagerActivityOutranksIdentity("图库", candidate) {
		t.Fatal("exact activity label did not outrank the package identity")
	}
	component, _, ok := selectPackageManagerLaunchComponent("图库", candidate.Activities)
	if !ok || component != "com.example.camera/com.example.camera.Gallery" {
		t.Fatalf("activity-label selection = %q, %v", component, ok)
	}
	if packageManagerActivityOutranksIdentity("相机", candidate) {
		t.Fatal("exact application label should keep the system default resolver path")
	}
}

func TestAppSearchPageHonorsPreviewBudgetAndAdvances(t *testing.T) {
	ranked := make([]scoredAppCandidate, 0, 50)
	for appIndex := 0; appIndex < 50; appIndex++ {
		candidate := androidAppCandidate{
			Package: "com.example.app" + strconv.Itoa(appIndex), Label: strings.Repeat("<", 256), Installed: true, Enabled: true,
		}
		for launcherIndex := 0; launcherIndex < 8; launcherIndex++ {
			candidate.Activities = append(candidate.Activities, androidLaunchCandidate{
				Kind: "launcher", Component: candidate.Package + "/" + candidate.Package + ".Main" + strconv.Itoa(launcherIndex),
				Label: strings.Repeat("<", 256), Enabled: true, Exported: true,
			})
		}
		ranked = append(ranked, scoredAppCandidate{candidate: candidate, score: 600})
	}
	const budget = 4 << 10
	offset := 0
	for offset < len(ranked) {
		page := buildAppSearchPage("<", "all", "0", len(ranked), offset, 50, ranked[offset:], false, budget)
		if size := jsonEncodedSize(page); size > appResultDataBudget(budget) {
			t.Fatalf("page at offset %d encoded to %d bytes", offset, size)
		}
		returned, ok := page["returned"].(int)
		if !ok || returned < 1 {
			t.Fatalf("page at offset %d did not advance: %+v", offset, page)
		}
		offset += returned
		if offset < len(ranked) {
			if next, ok := page["nextOffset"].(int); !ok || next != offset || page["hasMore"] != true {
				t.Fatalf("page continuation = %+v, want offset %d", page, offset)
			}
		} else if page["hasMore"] != false {
			t.Fatalf("last page still reports more results: %+v", page)
		}
	}
}

func TestAppCommandPreviewReservesMCPEnvelope(t *testing.T) {
	result := Result{Stdout: strings.Repeat("x", 4<<10), Data: map[string]any{"package": "com.example"}}
	fitAppCommandPreview(&result, 4<<10)
	if len(result.Stdout) > appResultDataBudget(4<<10) {
		t.Fatalf("stdout retained %d bytes", len(result.Stdout))
	}
	data := result.Data.(map[string]any)
	if data["outputTruncated"] != true || data["inlineLimitBytes"] != 4<<10 {
		t.Fatalf("truncation metadata = %+v", data)
	}
}

func TestAppSearchPageAdvancesWithMaximumEscapedQuery(t *testing.T) {
	packageName := "com." + strings.Repeat("a", 240)
	ranked := []scoredAppCandidate{{candidate: androidAppCandidate{Package: packageName, Label: strings.Repeat("<", 256), Installed: true, Enabled: true}, score: 900}}
	page := buildAppSearchPage(strings.Repeat("<", 256), "all", "0", 1, 0, 1, ranked, false, 4<<10)
	if page["returned"] != 1 || page["hasMore"] != false {
		t.Fatalf("maximum escaped query did not advance: %+v", page)
	}
	if size := jsonEncodedSize(page); size > appResultDataBudget(4<<10) {
		t.Fatalf("maximum escaped query page encoded to %d bytes", size)
	}
}

func TestParseAndroidUserIDIsStrict(t *testing.T) {
	if got, err := parseAndroidUserID("0\n"); err != nil || got != "0" {
		t.Fatalf("current user = %q, %v", got, err)
	}
	for _, output := range []string{"", "Current user: 10\nwarning 36", "0\n10\n", "warning 36\n"} {
		if _, err := parseAndroidUserID(output); err == nil {
			t.Errorf("ambiguous user output %q was accepted", output)
		}
	}
}

func TestEmbeddedAppQueryHelper(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(appQueryHelperBase64))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || len(raw) > 64<<10 {
		t.Fatalf("embedded helper size = %d", len(raw))
	}
	archive, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.File) != 1 || archive.File[0].Name != "classes.dex" {
		t.Fatalf("unexpected helper entries: %+v", archive.File)
	}
	reader, err := archive.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 8)
	_, readErr := io.ReadFull(reader, header)
	remainder, remainderErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil || remainderErr != nil || !bytes.HasPrefix(header, []byte("dex\n")) {
		t.Fatalf("embedded helper is not a DEX: %q, %v, %v", header, readErr, remainderErr)
	}
	dex := append(header, remainder...)
	for _, marker := range [][]byte{
		[]byte("application match limit exceeded"), []byte("package launcher limit exceeded"),
		[]byte("total launcher limit exceeded"), []byte("helper output limit exceeded"),
	} {
		if !bytes.Contains(dex, marker) {
			t.Fatalf("embedded helper is missing resource-limit marker %q", marker)
		}
	}
	if bytes.Contains(dex, []byte("isPackageSuspended")) {
		t.Fatal("embedded helper references an API unavailable on Android 8")
	}
	manifestRaw, err := os.ReadFile(filepath.Join("appquery", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		ProtocolVersion int    `json:"protocolVersion"`
		MinAPI          int    `json:"minApi"`
		SourceSHA256    string `json:"sourceSha256"`
		JarSHA256       string `json:"jarSha256"`
	}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join("..", "..", "scripts", "appquery", "AppQuery.java"))
	if err != nil {
		t.Fatal(err)
	}
	if sourceDigest := fmt.Sprintf("%x", sha256.Sum256(source)); sourceDigest != manifest.SourceSHA256 {
		t.Fatalf("PackageManager helper source drifted: %s", sourceDigest)
	}
	if jarDigest := fmt.Sprintf("%x", sha256.Sum256(raw)); jarDigest != manifest.JarSHA256 {
		t.Fatalf("embedded PackageManager helper drifted: %s", jarDigest)
	}
	if manifest.ProtocolVersion != appQueryProtocolVersion || manifest.MinAPI != 26 {
		t.Fatalf("unexpected PackageManager helper manifest: %+v", manifest)
	}
	manager := testManager(t)
	path, err := manager.materializeAppQueryHelper()
	if err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(written, raw) || filepath.Ext(path) != ".jar" {
		t.Fatalf("materialized helper mismatch: path=%q bytes=%d err=%v", path, len(written), err)
	}
}

func TestFullCommandStdoutUsesBoundedArtifact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "query.stdout")
	if err := os.WriteFile(path, []byte("V\t1\nT\t0\t0\t0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := fullCommandStdout(Result{Stdout: "V\t1\n[输出已截断，完整内容见 artifacts]", Artifacts: []string{path}}, 1024)
	if err != nil || output != "V\t1\nT\t0\t0\t0\n" {
		t.Fatalf("artifact output = %q, %v", output, err)
	}
	if _, err := fullCommandStdout(Result{Stdout: "[输出已截断]", Artifacts: []string{path}}, 4); err == nil {
		t.Fatal("oversized helper artifact was accepted")
	}
}
