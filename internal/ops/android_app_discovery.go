package ops

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zcr521/android-ai-mcp/internal/atomicfile"
	"golang.org/x/text/unicode/norm"
)

const (
	appQueryProtocolVersion = 1
	appQueryMaxOutputBytes  = 2 << 20
	appQueryMaxCandidates   = 64
	appQueryMaxMatched      = 4096
	appQueryMaxLaunchers    = 256
	appQueryMaxRecords      = 8192
)

//go:embed appquery/app-query.jar.b64
var appQueryHelperBase64 string

type androidLaunchCandidate struct {
	Kind           string
	Component      string
	Label          string
	Enabled        bool
	Exported       bool
	Target         string
	State          int
	TargetEnabled  bool
	TargetExported bool
	TargetState    int
}

type androidAppCandidate struct {
	Package    string
	Label      string
	System     bool
	Installed  bool
	Enabled    bool
	Suspended  bool
	State      int
	SourceDir  string
	Activities []androidLaunchCandidate
}

type resolvedAppTarget struct {
	Query             string
	Package           string
	Label             string
	User              string
	ExplicitComponent string
	Candidate         androidAppCandidate
}

func (m *Manager) queryAndroidApplications(ctx context.Context, user, query string, offset, limit, systemFilter int) ([]androidAppCandidate, int, bool, error) {
	query, err := normalizePackageManagerQuery(query)
	if err != nil || offset < 0 || offset > 100000 || limit < 1 || limit > appQueryMaxCandidates || systemFilter < -1 || systemFilter > 1 {
		return nil, 0, false, errors.New("invalid PackageManager query")
	}
	helperPath, err := m.materializeAppQueryHelper()
	if err != nil {
		return nil, 0, false, err
	}
	timeout := 8 * time.Second
	if m.cfg.ShellTimeout > 0 && m.cfg.ShellTimeout < timeout {
		timeout = m.cfg.ShellTimeout
	}
	result := m.runCommand(ctx, commandSpec{
		Name: "/system/bin/app_process",
		Args: []string{"/system/bin", "com.zcr521.mcp.AppQuery", user, query,
			strconv.Itoa(offset), strconv.Itoa(limit), strconv.Itoa(systemFilter)},
		Env:      map[string]string{"CLASSPATH": helperPath},
		Timeout:  timeout,
		Strategy: "android_package_manager_label_query",
	})
	output, readErr := fullCommandStdout(result, appQueryMaxOutputBytes)
	if readErr != nil {
		return nil, 0, false, readErr
	}
	candidates, matched, truncated, parseErr := parseAppQueryOutput(output)
	if parseErr != nil {
		return nil, 0, false, parseErr
	}
	if pageErr := validateAppQueryPage(offset, limit, matched, len(candidates), truncated); pageErr != nil {
		return nil, 0, false, pageErr
	}
	if !result.Success {
		return nil, 0, false, fmt.Errorf("PackageManager helper failed: %s", result.Code)
	}
	return candidates, matched, truncated, nil
}

func validateAppQueryPage(offset, limit, total, emitted int, hasMore bool) error {
	remaining := total - offset
	if remaining < 0 {
		remaining = 0
	}
	expected := minInt(limit, remaining)
	if emitted != expected || hasMore != (offset+emitted < total) {
		return errors.New("PackageManager helper returned inconsistent pagination metadata")
	}
	return nil
}

func (m *Manager) materializeAppQueryHelper() (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(appQueryHelperBase64))
	if err != nil || len(raw) == 0 || len(raw) > 64<<10 {
		return "", errors.New("embedded PackageManager helper is invalid")
	}
	digest := sha256.Sum256(raw)
	name := fmt.Sprintf("app-query-v%d-%x.jar", appQueryProtocolVersion, digest[:6])
	path := filepath.Join(m.cfg.StateDir, "runtime", name)
	if info, statErr := os.Stat(path); statErr == nil && info.Mode().IsRegular() && info.Size() == int64(len(raw)) {
		existing, readErr := os.ReadFile(path)
		if readErr == nil && bytes.Equal(existing, raw) {
			return path, nil
		}
	}
	if err := atomicfile.Write(path, raw, 0o600); err != nil {
		return "", fmt.Errorf("write PackageManager helper: %w", err)
	}
	return path, nil
}

func fullCommandStdout(result Result, limit int64) (string, error) {
	if !strings.Contains(result.Stdout, "[输出已截断") {
		if int64(len(result.Stdout)) > limit {
			return "", errors.New("PackageManager helper output exceeded its limit")
		}
		return result.Stdout, nil
	}
	for _, artifact := range result.Artifacts {
		if !strings.HasSuffix(artifact, ".stdout") {
			continue
		}
		file, err := os.Open(artifact)
		if err != nil {
			return "", err
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, limit+1))
		_ = file.Close()
		if readErr != nil {
			return "", readErr
		}
		if int64(len(raw)) > limit {
			return "", errors.New("PackageManager helper output exceeded its limit")
		}
		return string(raw), nil
	}
	return "", errors.New("PackageManager helper output was truncated without a readable artifact")
}

func parseAppQueryOutput(output string) ([]androidAppCandidate, int, bool, error) {
	apps := make(map[string]*androidAppCandidate)
	order := make([]string, 0)
	versionSeen := false
	summarySeen := false
	truncated := false
	matchedTotal := 0
	lineCount := 0
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 4096), 64<<10)
	for scanner.Scan() {
		lineCount++
		if lineCount > appQueryMaxRecords {
			return nil, 0, false, errors.New("PackageManager helper returned too many records")
		}
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) == 0 {
			continue
		}
		if summarySeen {
			return nil, 0, false, errors.New("PackageManager helper returned records after its summary")
		}
		switch fields[0] {
		case "V":
			if len(fields) != 2 || fields[1] != strconv.Itoa(appQueryProtocolVersion) || versionSeen {
				return nil, 0, false, errors.New("PackageManager helper protocol mismatch")
			}
			versionSeen = true
		case "A":
			if !versionSeen || len(fields) != 9 {
				return nil, 0, false, errors.New("invalid PackageManager application record")
			}
			pkg, err := decodeAppQueryField(fields[1])
			if err != nil || !looksLikePackage(pkg) {
				return nil, 0, false, errors.New("invalid PackageManager package record")
			}
			if _, exists := apps[pkg]; exists {
				return nil, 0, false, errors.New("duplicate PackageManager package record")
			}
			if len(order) >= appQueryMaxCandidates {
				return nil, 0, false, errors.New("PackageManager helper returned too many applications")
			}
			label, err := decodeAppQueryField(fields[2])
			if err != nil || label == "" || !utf8.ValidString(label) {
				return nil, 0, false, errors.New("invalid PackageManager label record")
			}
			state, err := strconv.Atoi(fields[7])
			if err != nil || state < 0 || state > 4 {
				return nil, 0, false, errors.New("invalid PackageManager state record")
			}
			sourceDir, err := decodeAppQueryField(fields[8])
			if err != nil || len(sourceDir) > 4096 || (sourceDir != "" && (!strings.HasPrefix(sourceDir, "/") || strings.ContainsRune(sourceDir, '\x00'))) {
				return nil, 0, false, errors.New("invalid PackageManager source record")
			}
			candidate := &androidAppCandidate{
				Package: pkg, Label: label, System: fields[3] == "1", Installed: fields[4] == "1",
				Enabled: fields[5] == "1", Suspended: fields[6] == "1", State: state, SourceDir: sourceDir,
			}
			for _, index := range []int{3, 4, 5, 6} {
				if fields[index] != "0" && fields[index] != "1" {
					return nil, 0, false, errors.New("invalid PackageManager boolean record")
				}
			}
			if !candidate.Installed {
				return nil, 0, false, errors.New("PackageManager helper returned an uninstalled application")
			}
			apps[pkg] = candidate
			order = append(order, pkg)
		case "L":
			if !versionSeen || len(fields) != 12 {
				return nil, 0, false, errors.New("invalid PackageManager launcher record")
			}
			pkg, err := decodeAppQueryField(fields[1])
			candidate := apps[pkg]
			if err != nil || candidate == nil {
				return nil, 0, false, errors.New("orphan PackageManager launcher record")
			}
			kind, _ := decodeAppQueryField(fields[2])
			component, componentErr := decodeAppQueryField(fields[3])
			component, componentPackage, componentErr := normalizeAndroidComponent(component)
			if componentErr != nil || componentPackage != pkg || (kind != "launcher" && kind != "leanback" && kind != "info") {
				return nil, 0, false, errors.New("invalid PackageManager launcher component")
			}
			label, err := decodeAppQueryField(fields[4])
			if err != nil || label == "" || !utf8.ValidString(label) {
				return nil, 0, false, errors.New("invalid PackageManager launcher label")
			}
			target, err := decodeAppQueryField(fields[7])
			if err != nil {
				return nil, 0, false, errors.New("invalid PackageManager target component")
			}
			if target != "" {
				target, componentPackage, err = normalizeAndroidComponent(target)
				if err != nil || componentPackage != pkg {
					return nil, 0, false, errors.New("invalid PackageManager target component")
				}
			}
			state, stateErr := strconv.Atoi(fields[8])
			targetState, targetStateErr := strconv.Atoi(fields[11])
			if stateErr != nil || targetStateErr != nil || state < 0 || state > 4 || targetState < 0 || targetState > 4 {
				return nil, 0, false, errors.New("invalid PackageManager component state")
			}
			for _, index := range []int{5, 6, 9, 10} {
				if fields[index] != "0" && fields[index] != "1" {
					return nil, 0, false, errors.New("invalid PackageManager component boolean")
				}
			}
			for _, existing := range candidate.Activities {
				if existing.Component == component {
					return nil, 0, false, errors.New("duplicate PackageManager launcher record")
				}
			}
			if len(candidate.Activities) >= appQueryMaxLaunchers {
				return nil, 0, false, errors.New("PackageManager helper returned too many launchers")
			}
			candidate.Activities = append(candidate.Activities, androidLaunchCandidate{
				Kind: kind, Component: component, Label: label, Enabled: fields[5] == "1", Exported: fields[6] == "1",
				Target: target, State: state, TargetEnabled: fields[9] == "1", TargetExported: fields[10] == "1", TargetState: targetState,
			})
		case "T":
			if !versionSeen || summarySeen || len(fields) != 4 || fields[3] != "0" && fields[3] != "1" {
				return nil, 0, false, errors.New("invalid PackageManager summary record")
			}
			matched, matchedErr := strconv.Atoi(fields[1])
			emitted, emittedErr := strconv.Atoi(fields[2])
			if matchedErr != nil || emittedErr != nil || matched < 0 || matched > appQueryMaxMatched || emitted < 0 || emitted != len(order) || matched < emitted {
				return nil, 0, false, errors.New("invalid PackageManager summary counts")
			}
			summarySeen = true
			matchedTotal = matched
			truncated = fields[3] == "1"
		case "E":
			if len(fields) != 3 {
				return nil, 0, false, errors.New("PackageManager helper failed")
			}
			kind, kindErr := decodeAppQueryField(fields[1])
			message, messageErr := decodeAppQueryField(fields[2])
			if kindErr != nil || messageErr != nil {
				return nil, 0, false, errors.New("PackageManager helper returned an invalid error record")
			}
			return nil, 0, false, fmt.Errorf("PackageManager helper failed: %s: %s", truncateUTF8Bytes(kind, 128), truncateUTF8Bytes(message, 256))
		default:
			return nil, 0, false, errors.New("unknown PackageManager helper record")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, false, fmt.Errorf("read PackageManager helper output: %w", err)
	}
	if !versionSeen || !summarySeen {
		return nil, 0, false, errors.New("PackageManager helper returned an incomplete response")
	}
	result := make([]androidAppCandidate, 0, len(order))
	for _, pkg := range order {
		result = append(result, *apps[pkg])
	}
	return result, matchedTotal, truncated, nil
}

func decodeAppQueryField(value string) (string, error) {
	if value == "-" {
		return "", nil
	}
	if value == "" || len(value) > 16<<10 {
		return "", errors.New("invalid encoded field")
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || !utf8.Valid(raw) {
		return "", errors.New("invalid encoded field")
	}
	return string(raw), nil
}

func normalizeAppQuery(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(norm.NFKC.String(strings.TrimSpace(value)))), " ")
}

func normalizePackageManagerQuery(value string) (string, error) {
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return "", errors.New("application query is not valid text")
	}
	value = strings.TrimSpace(norm.NFKC.String(value))
	if value == "" || utf8.RuneCountInString(value) > 256 {
		return "", errors.New("application query must contain 1..256 Unicode characters")
	}
	return value, nil
}

func appCandidateScore(query string, candidate androidAppCandidate) int {
	query = normalizeAppQuery(query)
	if query == "" {
		return 0
	}
	score := appIdentityScore(query, candidate)
	for _, activity := range candidate.Activities {
		if activityScore := appActivityScore(query, activity); activityScore > score {
			score = activityScore
		}
	}
	return score
}

func appIdentityScore(query string, candidate androidAppCandidate) int {
	packageName := normalizeAppQuery(candidate.Package)
	label := normalizeAppQuery(candidate.Label)
	switch {
	case packageName == query:
		return 1000
	case label == query:
		return 900
	case packageSegmentMatch(packageName, query):
		return 800
	case strings.HasPrefix(label, query):
		return 700
	case strings.HasPrefix(packageName, query):
		return 650
	case strings.Contains(label, query):
		return 600
	case strings.Contains(packageName, query):
		return 500
	}
	return 0
}

func appActivityScore(query string, activity androidLaunchCandidate) int {
	activityLabel := normalizeAppQuery(activity.Label)
	activityComponent := normalizeAppQuery(activity.Component)
	switch {
	case activityLabel == query:
		return 850
	case strings.HasPrefix(activityLabel, query):
		return 675
	case strings.Contains(activityLabel, query):
		return 575
	case strings.Contains(activityComponent, query):
		return 475
	}
	return 0
}

func packageSegmentMatch(packageName, query string) bool {
	for _, segment := range strings.Split(packageName, ".") {
		if segment == query {
			return true
		}
	}
	return false
}

func selectAppCandidate(query string, candidates []androidAppCandidate) (androidAppCandidate, []androidAppCandidate, error) {
	items := rankAppCandidates(query, candidates)
	if len(items) == 0 {
		return androidAppCandidate{}, nil, os.ErrNotExist
	}
	top := items[0].score
	ambiguous := make([]androidAppCandidate, 0)
	for _, item := range items {
		if item.score != top {
			break
		}
		ambiguous = append(ambiguous, item.candidate)
	}
	if len(ambiguous) > 1 {
		return androidAppCandidate{}, ambiguous, errors.New("application name is ambiguous")
	}
	return items[0].candidate, nil, nil
}

type scoredAppCandidate struct {
	candidate androidAppCandidate
	score     int
}

func rankAppCandidates(query string, candidates []androidAppCandidate) []scoredAppCandidate {
	items := make([]scoredAppCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if score := appCandidateScore(query, candidate); score > 0 {
			items = append(items, scoredAppCandidate{candidate: candidate, score: score})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		return items[i].candidate.Package < items[j].candidate.Package
	})
	return items
}

func normalizeAndroidComponent(value string) (string, string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\x00\r\n\t ") || strings.Count(value, "/") != 1 {
		return "", "", errors.New("component 必须形如 package/.Class")
	}
	parts := strings.SplitN(value, "/", 2)
	pkg := parts[0]
	if !looksLikePackage(pkg) {
		return "", "", errors.New("component 包名无效")
	}
	className := parts[1]
	if strings.HasPrefix(className, ".") {
		className = pkg + className
	}
	if className == "" || strings.HasPrefix(className, ".") || strings.HasSuffix(className, ".") || strings.Contains(className, "..") {
		return "", "", errors.New("component 类名无效")
	}
	for _, character := range className {
		if character != '.' && character != '_' && character != '$' &&
			(character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') {
			return "", "", errors.New("component 类名无效")
		}
	}
	return pkg + "/" + className, pkg, nil
}

func appCandidateSummary(candidate androidAppCandidate) map[string]any {
	components := make([]map[string]any, 0, minInt(len(candidate.Activities), 8))
	for index, activity := range candidate.Activities {
		if index >= 8 {
			break
		}
		components = append(components, map[string]any{
			"component": activity.Component, "label": activity.Label, "kind": activity.Kind,
			"enabled": androidEffectiveEnabled(activity.Enabled, activity.State), "exported": activity.Exported,
		})
	}
	return map[string]any{
		"package": candidate.Package, "label": candidate.Label, "system": candidate.System,
		"installed": candidate.Installed, "enabled": androidEffectiveEnabled(candidate.Enabled, candidate.State), "suspended": candidate.Suspended,
		"launchers": components,
	}
}

func ambiguousAppFailure(query string, candidates []androidAppCandidate, truncated bool) Result {
	items := make([]map[string]any, 0, minInt(len(candidates), 20))
	for index, candidate := range candidates {
		if index >= 20 {
			break
		}
		items = append(items, appCandidateSummary(candidate))
	}
	result := fail("AMBIGUOUS_APP", "应用名称对应多个已安装应用，请从候选中选择", errors.New("ambiguous application name"), "android_package_manager_label_query")
	result.Data = map[string]any{"query": query, "candidates": items, "truncated": truncated || len(candidates) > len(items)}
	return result
}

func (m *Manager) resolveAppUser(ctx context.Context, args map[string]any, allowAll bool) (string, error) {
	user, err := androidUser(args)
	if err != nil {
		return "", err
	}
	if user == "all" {
		if allowAll {
			return user, nil
		}
		return "", errors.New("user=all is not allowed for this operation")
	}
	if user != "current" {
		return user, nil
	}
	commands := []commandVariant{
		{Name: "cmd", Args: []string{"activity", "get-current-user"}, Strategy: "cmd_activity_current_user"},
		{Name: "am", Args: []string{"get-current-user"}, Strategy: "am_current_user"},
	}
	var lastErr error
	for _, command := range commands {
		result := m.runAndroid(ctx, command)
		if !result.Success {
			lastErr = errors.New(result.Error)
			continue
		}
		if id, parseErr := parseAndroidUserID(result.Stdout); parseErr == nil {
			return id, nil
		} else {
			lastErr = parseErr
		}
	}
	if lastErr == nil {
		lastErr = errors.New("current user command failed")
	}
	return "", lastErr
}

func parseAndroidUserID(output string) (string, error) {
	ids := make([]string, 0, 1)
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		id, err := strconv.Atoi(line)
		if err == nil && id >= 0 {
			ids = append(ids, strconv.Itoa(id))
		}
	}
	if len(ids) != 1 {
		return "", errors.New("current user command returned no unique numeric user ID")
	}
	return ids[0], nil
}

func (m *Manager) resolveAppTarget(ctx context.Context, args map[string]any, allowAll bool) (resolvedAppTarget, *Result) {
	user, err := m.resolveAppUser(ctx, args, allowAll)
	if err != nil {
		result := fail("USER_RESOLUTION_FAILED", "无法确定应用所属 Android 用户", err, "android_current_user_probe")
		return resolvedAppTarget{}, &result
	}
	explicitComponent, err := argOptionalString(args, "", "component")
	if err != nil {
		result := invalid(err.Error())
		return resolvedAppTarget{}, &result
	}
	componentPackage := ""
	if explicitComponent != "" {
		explicitComponent, componentPackage, err = normalizeAndroidComponent(explicitComponent)
		if err != nil {
			result := invalid(err.Error())
			return resolvedAppTarget{}, &result
		}
	}
	rawPackage, err := argOptionalString(args, "", "package", "packageName")
	if err != nil {
		result := invalid(err.Error())
		return resolvedAppTarget{}, &result
	}
	query, err := argOptionalString(args, "", "query", "appName", "name")
	if err != nil {
		result := invalid(err.Error())
		return resolvedAppTarget{}, &result
	}
	rawPackage = strings.TrimSpace(rawPackage)
	query = strings.TrimSpace(query)
	packageTarget := ""
	if rawPackage != "" {
		if !looksLikePackage(rawPackage) {
			result := invalid("package/packageName 必须是合法包名；按显示名查询请使用 query/appName/name")
			return resolvedAppTarget{}, &result
		}
		packageTarget = rawPackage
	}
	if componentPackage != "" {
		if packageTarget != "" && packageTarget != componentPackage {
			result := invalid("component 与 package/packageName 不属于同一应用")
			return resolvedAppTarget{}, &result
		}
		packageTarget = componentPackage
	}
	if packageTarget == "" && query == "" {
		result := fail("APP_QUERY_REQUIRED", "请提供应用显示名、包名或组件", errors.New("application target is missing"), "app_target_validation")
		return resolvedAppTarget{}, &result
	}
	if user == "all" && packageTarget == "" {
		result := invalid("按应用显示名查询时必须指定 current 或单个 Android 用户")
		return resolvedAppTarget{}, &result
	}
	if query != "" {
		query, err = normalizePackageManagerQuery(query)
		if err != nil {
			result := invalid("应用名称必须是 1..256 个 Unicode 字符的有效文本")
			return resolvedAppTarget{}, &result
		}
	}
	lookup := query
	var candidate androidAppCandidate
	if packageTarget != "" {
		lookup = packageTarget
		candidate = androidAppCandidate{Package: packageTarget, Label: packageTarget, Installed: true, Enabled: true}
	} else {
		candidates, _, truncated, queryErr := m.queryAndroidApplications(ctx, user, lookup, 0, appQueryMaxCandidates, 0)
		if queryErr != nil {
			result := fail("APP_DISCOVERY_UNAVAILABLE", "系统 PackageManager 无法按应用名称查询", queryErr, "android_package_manager_label_query")
			return resolvedAppTarget{}, &result
		}
		selected, ambiguous, selectErr := selectAppCandidate(query, candidates)
		if len(ambiguous) > 1 {
			result := ambiguousAppFailure(query, ambiguous, truncated)
			return resolvedAppTarget{}, &result
		}
		if selectErr != nil {
			result := fail("APP_NOT_FOUND", "当前用户中未找到匹配的已安装应用", os.ErrNotExist, "android_package_manager_label_query")
			result.Data = map[string]any{"query": query, "user": user, "truncated": truncated}
			return resolvedAppTarget{}, &result
		}
		candidate = selected
		packageTarget = candidate.Package
	}
	if user != "all" {
		paths, pathErr := m.packagePathsForUser(ctx, user, packageTarget)
		if pathErr != nil || len(paths) == 0 {
			result := fail("APP_NOT_INSTALLED_FOR_USER", "应用存在，但未安装到目标 Android 用户或当前不可用", os.ErrNotExist, "android_package_path_readback")
			result.Data = map[string]any{"query": lookup, "package": packageTarget, "user": user}
			return resolvedAppTarget{}, &result
		}
		if candidate.SourceDir == "" {
			candidate.SourceDir = paths[0]
		}
	}
	candidate.Installed = true
	return resolvedAppTarget{Query: lookup, Package: packageTarget, Label: candidate.Label, User: user, ExplicitComponent: explicitComponent, Candidate: candidate}, nil
}

func (m *Manager) packagePathsForUser(ctx context.Context, user, pkg string) ([]string, error) {
	commands := []commandVariant{
		{Name: "cmd", Args: []string{"package", "path", "--user", user, pkg}, Strategy: "cmd_package_path_user"},
		{Name: "pm", Args: []string{"path", "--user", user, pkg}, Strategy: "pm_path_user"},
	}
	var last Result
	for _, command := range commands {
		last = m.runAndroid(ctx, command)
		if last.Success {
			if paths := parsePMPaths(last.Stdout); len(paths) > 0 {
				return paths, nil
			}
		}
	}
	if last.Error != "" {
		return nil, errors.New(last.Error)
	}
	return nil, os.ErrNotExist
}

func (m *Manager) resolveLaunchComponent(ctx context.Context, target resolvedAppTarget) (string, string, error) {
	if target.Candidate.Package != "" && (!androidEffectiveEnabled(target.Candidate.Enabled, target.Candidate.State) || target.Candidate.Suspended) {
		return "", "", errors.New("installed application is disabled or suspended")
	}
	if target.ExplicitComponent != "" {
		return target.ExplicitComponent, "explicit_component", nil
	}
	if packageManagerActivityOutranksIdentity(target.Query, target.Candidate) {
		if component, strategy, ok := selectPackageManagerLaunchComponent(target.Query, target.Candidate.Activities); ok {
			return component, strategy, nil
		}
	}
	for _, category := range []struct {
		name string
		kind string
	}{
		{name: "android.intent.category.LAUNCHER", kind: "launcher"},
		{name: "android.intent.category.LEANBACK_LAUNCHER", kind: "leanback"},
		{name: "android.intent.category.INFO", kind: "info"},
	} {
		commands := []commandVariant{
			{Name: "cmd", Args: []string{"package", "resolve-activity", "--brief", "--components", "--user", target.User,
				"-a", "android.intent.action.MAIN", "-c", category.name, "-p", target.Package}, Strategy: "cmd_package_resolve_" + category.kind},
			{Name: "pm", Args: []string{"resolve-activity", "--brief", "--components", "--user", target.User,
				"-a", "android.intent.action.MAIN", "-c", category.name, "-p", target.Package}, Strategy: "pm_resolve_" + category.kind},
		}
		for _, command := range commands {
			result := m.runAndroid(ctx, command)
			if component, ok := parseResolvedComponent(result.Stdout, target.Package); result.Success && ok {
				if resolvedComponentUsable(target.Candidate, component) {
					return component, command.Strategy, nil
				}
			}
		}
	}
	if len(target.Candidate.Activities) == 0 {
		candidates, _, _, queryErr := m.queryAndroidApplications(ctx, target.User, target.Package, 0, appQueryMaxCandidates, 0)
		if queryErr == nil {
			for _, candidate := range candidates {
				if candidate.Package == target.Package {
					target.Candidate = candidate
					break
				}
			}
		}
	}
	if target.Candidate.Package != "" && (!androidEffectiveEnabled(target.Candidate.Enabled, target.Candidate.State) || target.Candidate.Suspended) {
		return "", "", errors.New("installed application is disabled or suspended")
	}
	if component, strategy, ok := selectPackageManagerLaunchComponent(target.Query, target.Candidate.Activities); ok {
		return component, strategy, nil
	}
	return "", "", errors.New("installed package has no enabled launchable activity")
}

func packageManagerActivityOutranksIdentity(query string, candidate androidAppCandidate) bool {
	query = normalizeAppQuery(query)
	identityScore := appIdentityScore(query, candidate)
	for _, activity := range candidate.Activities {
		if appActivityScore(query, activity) > identityScore {
			return true
		}
	}
	return false
}

func resolvedComponentUsable(candidate androidAppCandidate, component string) bool {
	if len(candidate.Activities) == 0 {
		return true
	}
	for _, activity := range candidate.Activities {
		if activity.Component == component {
			return androidEffectiveEnabled(activity.Enabled, activity.State) && activity.Exported
		}
		if activity.Target == component {
			return androidEffectiveEnabled(activity.TargetEnabled, activity.TargetState) && activity.TargetExported
		}
	}
	return false
}

func selectPackageManagerLaunchComponent(query string, candidates []androidLaunchCandidate) (string, string, bool) {
	query = normalizeAppQuery(query)
	activities := append([]androidLaunchCandidate(nil), candidates...)
	sort.SliceStable(activities, func(i, j int) bool {
		left := launchCandidateRank(query, activities[i])
		right := launchCandidateRank(query, activities[j])
		if left != right {
			return left > right
		}
		return activities[i].Component < activities[j].Component
	})
	for _, activity := range activities {
		if androidEffectiveEnabled(activity.Enabled, activity.State) && activity.Exported {
			return activity.Component, "package_manager_" + activity.Kind, true
		}
		if activity.Target != "" && androidEffectiveEnabled(activity.TargetEnabled, activity.TargetState) && activity.TargetExported {
			return activity.Target, "package_manager_disabled_alias_target", true
		}
	}
	return "", "", false
}

func androidEffectiveEnabled(manifestEnabled bool, state int) bool {
	switch state {
	case 1:
		return true
	case 2, 3, 4:
		return false
	default:
		return manifestEnabled
	}
}

func launchCandidateRank(query string, candidate androidLaunchCandidate) int {
	rank := appActivityScore(query, candidate) * 1000
	switch candidate.Kind {
	case "launcher":
		rank += 300
	case "leanback":
		rank += 200
	case "info":
		rank += 100
	}
	if androidEffectiveEnabled(candidate.Enabled, candidate.State) && candidate.Exported {
		rank += 50
	} else if androidEffectiveEnabled(candidate.TargetEnabled, candidate.TargetState) && candidate.TargetExported {
		rank += 25
	}
	return rank
}

func androidWaitStartError(result Result) error {
	if err := androidSemanticError(result); err != nil {
		return err
	}
	for _, rawLine := range strings.Split(strings.ToLower(result.Stdout+"\n"+result.Stderr), "\n") {
		line := strings.TrimSpace(rawLine)
		if strings.HasPrefix(line, "status:") && strings.TrimSpace(strings.TrimPrefix(line, "status:")) == "ok" {
			return nil
		}
	}
	return errors.New("Activity Manager returned no successful completion status")
}

func parseResolvedComponent(output, expectedPackage string) (string, bool) {
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		component, pkg, err := normalizeAndroidComponent(line)
		if err == nil && pkg == expectedPackage {
			return component, true
		}
	}
	return "", false
}
