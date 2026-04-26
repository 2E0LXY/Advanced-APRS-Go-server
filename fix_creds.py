import subprocess

# ── aprs_server.go ─────────────────────────────────────────────────────────
src = open(r'C:\aprs-fix\aprs_server.go', encoding='utf-8').read()

# 2e0lxy only appears in the admin username init and log messages
# Replace hardcoded username init with empty (first-run setup handles it)
src = src.replace(
    'adminCreds.Username = "2e0lxy"\n\tadminCreds.Password = "33wf31ug33"',
    '// Admin credentials loaded from creds.json by loadOrInitCreds()'
)
# Remove any stray literal references in log messages
src = src.replace('"Admin credentials loaded for user: %s"', '"Admin credentials loaded: %s"')

# NOCALL is fine as a default sentinel - it's intentional, not personal data
# Just verify it's only in config defaults
nocall_count = src.count('NOCALL')
print(f"NOCALL occurrences: {nocall_count} (expected: 1 in default config)")
for i, line in enumerate(src.split('\n')):
    if 'NOCALL' in line:
        print(f"  line {i+1}: {line.strip()}")

open(r'C:\aprs-fix\aprs_server.go', 'w', encoding='utf-8').write(src)
print("go saved")

# ── index.html ─────────────────────────────────────────────────────────────
html = open(r'C:\aprs-fix\index.html', encoding='utf-8').read()

# Find all 2e0lxy occurrences
lines_with = [(i+1, l.strip()) for i,l in enumerate(html.split('\n')) if '2e0lxy' in l.lower()]
print(f"\nindex.html 2e0lxy occurrences ({len(lines_with)}):")
for ln, l in lines_with:
    print(f"  line {ln}: {l[:100]}")
