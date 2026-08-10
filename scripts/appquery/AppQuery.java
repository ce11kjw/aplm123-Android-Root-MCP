package com.zcr521.mcp;

import android.content.ComponentName;
import android.content.Context;
import android.content.Intent;
import android.content.pm.ActivityInfo;
import android.content.pm.ApplicationInfo;
import android.content.pm.PackageManager;
import android.content.pm.ResolveInfo;
import android.os.UserHandle;
import android.os.Looper;
import android.util.Base64;

import java.lang.reflect.Method;
import java.nio.charset.StandardCharsets;
import java.text.Normalizer;
import java.util.ArrayList;
import java.util.Collections;
import java.util.Comparator;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;

public final class AppQuery {
    private static final int MAX_PAGE_SIZE = 64;
    private static final int MAX_MATCHED_APPLICATIONS = 4096;
    private static final int MAX_TOTAL_LAUNCHERS = 4096;
    private static final int MAX_PACKAGE_LAUNCHERS = 256;
    private static final int MAX_OUTPUT_BYTES = 1 << 20;

    private static final class ApplicationEntry {
        ApplicationInfo application;
        String label;
        LinkedHashMap<String, LaunchEntry> launchers;
        int score;
    }

    private static final class LaunchEntry {
        String kind;
        String component;
        String label;
        boolean enabled;
        boolean exported;
        String target;
        int state;
        boolean targetEnabled;
        boolean targetExported;
        int targetState;
    }

    public static void main(String[] args) {
        try {
            if (Looper.myLooper() == null) {
                Looper.prepareMainLooper();
            }
            if (args.length != 5) {
                throw new IllegalArgumentException("expected user, query, offset, limit and system filter");
            }
            int user = Integer.parseInt(args[0]);
            int offset = Integer.parseInt(args[2]);
            int limit = Integer.parseInt(args[3]);
            int systemFilter = Integer.parseInt(args[4]);
            if (user < 0) {
                throw new IllegalArgumentException("invalid user");
            }
            String query = normalize(args[1]);
            if (query.isEmpty() || query.codePointCount(0, query.length()) > 256) {
                throw new IllegalArgumentException("invalid query");
            }
            if (offset < 0 || limit < 1 || limit > MAX_PAGE_SIZE || systemFilter < -1 || systemFilter > 1) {
                throw new IllegalArgumentException("invalid page or system filter");
            }
            Context context = contextForUser(user);
            PackageManager pm = context.getPackageManager();
            int flags = PackageManager.MATCH_DISABLED_COMPONENTS
                    | PackageManager.MATCH_DISABLED_UNTIL_USED_COMPONENTS
                    | PackageManager.MATCH_DIRECT_BOOT_AWARE
                    | PackageManager.MATCH_DIRECT_BOOT_UNAWARE
                    | PackageManager.MATCH_UNINSTALLED_PACKAGES;
            Map<String, LinkedHashMap<String, LaunchEntry>> launchers = collectLaunchers(pm, flags);
            List<ApplicationInfo> applications = pm.getInstalledApplications(flags);
            List<ApplicationEntry> matches = new ArrayList<>();
            for (ApplicationInfo application : applications) {
                if (application.packageName == null || application.sourceDir == null
                        || (application.flags & 0x00800000) == 0) {
                    continue;
                }
                boolean system = (application.flags & ApplicationInfo.FLAG_SYSTEM) != 0;
                if (systemFilter == 1 && !system || systemFilter == -1 && system) {
                    continue;
                }
                String label = applicationLabel(pm, application);
                LinkedHashMap<String, LaunchEntry> packageLaunchers = launchers.get(application.packageName);
                int score = matchScore(query, application.packageName, label, packageLaunchers);
                if (score <= 0) {
                    continue;
                }
                ApplicationEntry entry = new ApplicationEntry();
                entry.application = application;
                entry.label = label;
                entry.launchers = packageLaunchers;
                entry.score = score;
                matches.add(entry);
                if (matches.size() > MAX_MATCHED_APPLICATIONS) {
                    throw new IllegalStateException("application match limit exceeded");
                }
            }
            Collections.sort(matches, new Comparator<ApplicationEntry>() {
                @Override
                public int compare(ApplicationEntry left, ApplicationEntry right) {
                    if (left.score != right.score) {
                        return Integer.compare(right.score, left.score);
                    }
                    return left.application.packageName.compareTo(right.application.packageName);
                }
            });
            int matched = matches.size();
            int emitted = 0;
            int end = Math.min(matched, Math.addExact(offset, limit));
            StringBuilder output = new StringBuilder();
            int[] outputBytes = new int[] {0};
            appendRecord(output, outputBytes, "V\t1");
            for (int index = Math.min(offset, matched); index < end; index++) {
                ApplicationEntry match = matches.get(index);
                ApplicationInfo application = match.application;
                String label = match.label;
                LinkedHashMap<String, LaunchEntry> packageLaunchers = match.launchers;
                boolean suspended = (application.flags & 0x40000000) != 0;
                int appState = PackageManager.COMPONENT_ENABLED_STATE_DEFAULT;
                try {
                    appState = pm.getApplicationEnabledSetting(application.packageName);
                } catch (Throwable ignored) {
                }
                appendRecord(output, outputBytes, "A\t" + encode(application.packageName)
                        + "\t" + encode(label)
                        + "\t" + bool((application.flags & ApplicationInfo.FLAG_SYSTEM) != 0)
                        + "\t" + bool((application.flags & 0x00800000) != 0)
                        + "\t" + bool(application.enabled)
                        + "\t" + bool(suspended)
                        + "\t" + appState
                        + "\t" + encode(application.sourceDir));
                if (packageLaunchers != null) {
                    for (LaunchEntry entry : packageLaunchers.values()) {
                        appendRecord(output, outputBytes, "L\t" + encode(application.packageName)
                                + "\t" + encode(entry.kind)
                                + "\t" + encode(entry.component)
                                + "\t" + encode(entry.label)
                                + "\t" + bool(entry.enabled)
                                + "\t" + bool(entry.exported)
                                + "\t" + encode(entry.target)
                                + "\t" + entry.state
                                + "\t" + bool(entry.targetEnabled)
                                + "\t" + bool(entry.targetExported)
                                + "\t" + entry.targetState);
                    }
                }
                emitted++;
            }
            appendRecord(output, outputBytes, "T\t" + matched + "\t" + emitted + "\t" + bool(end < matched));
            System.out.print(output.toString());
        } catch (Throwable error) {
            Throwable cause = error;
            while (cause.getCause() != null && cause.getCause() != cause) {
                cause = cause.getCause();
            }
            System.out.println("E\t" + encode(cause.getClass().getName())
                    + "\t" + encode(safeLabel(cause.getMessage(), "helper failure")));
            System.exit(2);
        }
    }

    private static Context contextForUser(int user) throws Exception {
        Class<?> activityThread = Class.forName("android.app.ActivityThread");
        Method systemMain = activityThread.getDeclaredMethod("systemMain");
        Object thread = systemMain.invoke(null);
        Method getSystemContext = activityThread.getDeclaredMethod("getSystemContext");
        Context base = (Context) getSystemContext.invoke(thread);
        Method createContextAsUser = Context.class.getDeclaredMethod(
                "createContextAsUser", UserHandle.class, int.class);
        return (Context) createContextAsUser.invoke(base, userHandle(user), 0);
    }

    private static UserHandle userHandle(int user) throws Exception {
        return UserHandle.getUserHandleForUid(Math.multiplyExact(user, 100000));
    }

    private static Map<String, LinkedHashMap<String, LaunchEntry>> collectLaunchers(PackageManager pm, int flags) {
        Map<String, LinkedHashMap<String, LaunchEntry>> output = new LinkedHashMap<>();
        int[] total = new int[] {0};
        collect(pm, flags, Intent.CATEGORY_LAUNCHER, "launcher", output, total);
        collect(pm, flags, Intent.CATEGORY_LEANBACK_LAUNCHER, "leanback", output, total);
        collect(pm, flags, Intent.CATEGORY_INFO, "info", output, total);
        return output;
    }

    private static void collect(PackageManager pm, int flags, String category, String kind,
                                Map<String, LinkedHashMap<String, LaunchEntry>> output, int[] total) {
        Intent intent = new Intent(Intent.ACTION_MAIN);
        intent.addCategory(category);
        List<ResolveInfo> resolved;
        try {
            resolved = pm.queryIntentActivities(intent, flags);
        } catch (Throwable error) {
            return;
        }
        for (ResolveInfo resolveInfo : resolved) {
            ActivityInfo activity = resolveInfo.activityInfo;
            if (activity == null || activity.packageName == null || activity.name == null) {
                continue;
            }
            ComponentName component = new ComponentName(activity.packageName, activity.name);
            LinkedHashMap<String, LaunchEntry> packageEntries = output.get(activity.packageName);
            if (packageEntries == null) {
                packageEntries = new LinkedHashMap<>();
                output.put(activity.packageName, packageEntries);
            }
            LaunchEntry existing = packageEntries.get(component.flattenToShortString());
            if (existing != null) {
                continue;
            }
            if (packageEntries.size() >= MAX_PACKAGE_LAUNCHERS) {
                throw new IllegalStateException("package launcher limit exceeded: " + activity.packageName);
            }
            if (total[0] >= MAX_TOTAL_LAUNCHERS) {
                throw new IllegalStateException("total launcher limit exceeded");
            }
            LaunchEntry entry = new LaunchEntry();
            entry.kind = kind;
            entry.component = component.flattenToShortString();
            entry.label = activityLabel(pm, resolveInfo, component.flattenToShortString());
            entry.enabled = activity.enabled && activity.applicationInfo != null && activity.applicationInfo.enabled;
            entry.exported = activity.exported;
            entry.target = activity.targetActivity == null ? "" :
                    new ComponentName(activity.packageName, activity.targetActivity).flattenToShortString();
            try {
                entry.state = pm.getComponentEnabledSetting(component);
            } catch (Throwable ignored) {
                entry.state = PackageManager.COMPONENT_ENABLED_STATE_DEFAULT;
            }
            if (!entry.target.isEmpty()) {
                try {
                    ComponentName targetComponent = ComponentName.unflattenFromString(entry.target);
                    ActivityInfo targetInfo = pm.getActivityInfo(targetComponent, flags);
                    entry.targetEnabled = targetInfo.enabled && targetInfo.applicationInfo != null
                            && targetInfo.applicationInfo.enabled;
                    entry.targetExported = targetInfo.exported;
                    entry.targetState = pm.getComponentEnabledSetting(targetComponent);
                } catch (Throwable ignored) {
                    entry.targetEnabled = false;
                    entry.targetExported = false;
                    entry.targetState = PackageManager.COMPONENT_ENABLED_STATE_DEFAULT;
                }
            }
            packageEntries.put(entry.component, entry);
            total[0]++;
        }
    }

    private static int matchScore(String query, String packageName, String label,
                                  LinkedHashMap<String, LaunchEntry> launchers) {
        String normalizedPackage = normalize(packageName);
        String normalizedLabel = normalize(label);
        int score = 0;
        if (normalizedPackage.equals(query)) {
            score = 1000;
        } else if (normalizedLabel.equals(query)) {
            score = 900;
        } else if (packageSegmentMatch(normalizedPackage, query)) {
            score = 800;
        } else if (normalizedLabel.startsWith(query)) {
            score = 700;
        } else if (normalizedPackage.startsWith(query)) {
            score = 650;
        } else if (normalizedLabel.contains(query)) {
            score = 600;
        } else if (normalizedPackage.contains(query)) {
            score = 500;
        }
        if (launchers != null) {
            for (LaunchEntry entry : launchers.values()) {
                String activityLabel = normalize(entry.label);
                String component = normalize(entry.component);
                int activityScore = 0;
                if (activityLabel.equals(query)) {
                    activityScore = 850;
                } else if (activityLabel.startsWith(query)) {
                    activityScore = 675;
                } else if (activityLabel.contains(query)) {
                    activityScore = 575;
                } else if (component.contains(query)) {
                    activityScore = 475;
                }
                score = Math.max(score, activityScore);
            }
        }
        return score;
    }

    private static boolean packageSegmentMatch(String packageName, String query) {
        String[] segments = packageName.split("\\.");
        for (String segment : segments) {
            if (segment.equals(query)) {
                return true;
            }
        }
        return false;
    }

    private static String normalize(String value) {
        if (value == null) {
            return "";
        }
        String normalized = Normalizer.normalize(value, Normalizer.Form.NFKC)
                .toLowerCase(Locale.ROOT).trim();
        return normalized.replaceAll("\\s+", " ");
    }

    private static String safeLabel(CharSequence value, String fallback) {
        String label = value == null ? "" : value.toString().trim();
        if (label.isEmpty()) {
            label = fallback;
        }
        if (label.length() > 256) {
            label = label.substring(0, 256);
        }
        return label;
    }

    private static String applicationLabel(PackageManager pm, ApplicationInfo application) {
        try {
            return safeLabel(application.loadLabel(pm), application.packageName);
        } catch (Throwable ignored) {
            return safeLabel(null, application.packageName);
        }
    }

    private static String activityLabel(PackageManager pm, ResolveInfo resolveInfo, String fallback) {
        try {
            return safeLabel(resolveInfo.loadLabel(pm), fallback);
        } catch (Throwable ignored) {
            return safeLabel(null, fallback);
        }
    }

    private static String encode(String value) {
        if (value == null || value.isEmpty()) {
            return "-";
        }
        return Base64.encodeToString(value.getBytes(StandardCharsets.UTF_8),
                Base64.URL_SAFE | Base64.NO_WRAP | Base64.NO_PADDING);
    }

    private static void appendRecord(StringBuilder output, int[] outputBytes, String record) {
        int bytes = record.getBytes(StandardCharsets.UTF_8).length + 1;
        if (bytes > MAX_OUTPUT_BYTES - outputBytes[0]) {
            throw new IllegalStateException("helper output limit exceeded");
        }
        output.append(record).append('\n');
        outputBytes[0] += bytes;
    }

    private static String bool(boolean value) {
        return value ? "1" : "0";
    }
}
