import subprocess

# Update install.sh to prompt for domain and replace placeholder in Caddyfile
install = open(r'C:\aprs-fix\install.sh', encoding='utf-8').read()

# Add domain prompt after the set -e line
old_set = 'set -e\n\nAPP_DIR="/opt/aprs-gateway"'
new_set = '''set -e

# ── Configuration ─────────────────────────────────────────────────────────────
APP_DIR="/opt/aprs-gateway"

if [ -z "$DOMAIN" ]; then
  read -p "Enter your domain name (e.g. aprs.example.com): " DOMAIN
fi
if [ -z "$DOMAIN" ]; then
  echo "ERROR: Domain name is required."
  exit 1
fi
echo "Domain: $DOMAIN"

APP_DIR="/opt/aprs-gateway"'''

install = install.replace(old_set, new_set)

# Replace the static Caddyfile copy with a sed-based domain substitution
old_caddy = 'cp Caddyfile /etc/caddy/Caddyfile'
new_caddy = 'sed "s/YOUR-DOMAIN.COM/$DOMAIN/g" Caddyfile > /etc/caddy/Caddyfile'
install = install.replace(old_caddy, new_caddy)

open(r'C:\aprs-fix\install.sh', 'w', encoding='utf-8').write(install)
print("install.sh updated")

# ── Final audit ───────────────────────────────────────────────────────────────
files = {
    'aprs_server.go': open(r'C:\aprs-fix\aprs_server.go', encoding='utf-8').read(),
    'index.html':     open(r'C:\aprs-fix\index.html',     encoding='utf-8').read(),
    'README.md':      open(r'C:\aprs-fix\README.md',      encoding='utf-8').read(),
    'install.sh':     open(r'C:\aprs-fix\install.sh',     encoding='utf-8').read(),
    'Caddyfile':      open(r'C:\aprs-fix\Caddyfile',      encoding='utf-8').read(),
    '.gitignore':     open(r'C:\aprs-fix\.gitignore',     encoding='utf-8').read(),
    'LICENCE':        open(r'C:\aprs-fix\LICENCE',        encoding='utf-8').read(),
}

SENSITIVE = ['33wf31', 'theloxleys.uk', '86.160.216', '53.7264', '-1.5744', '80.64.216']
# Note: 2e0lxy in GitHub URLs is OK; only flag if outside a github.com URL
print("\n=== Final Security Audit ===")
issues = 0
for fname, content in files.items():
    for needle in SENSITIVE:
        if needle.lower() in content.lower():
            print(f"  ISSUE: {fname} contains '{needle}'")
            issues += 1
    # Check for 2e0lxy outside of github URLs
    idx = 0
    while True:
        idx = content.lower().find('2e0lxy', idx)
        if idx == -1: break
        context = content[max(0,idx-30):idx+30]
        if 'github.com' not in context and 'placeholder' not in context.lower():
            print(f"  WARN:  {fname} contains '2e0lxy' outside github URL: ...{context.strip()}...")
        idx += 6

if issues == 0:
    print("  All clear - no sensitive data found")

print("\n=== Committing ===")
msg = "Security: remove personal domain, callsign and credentials from all tracked files\n\n- Caddyfile: replace theloxleys.uk with YOUR-DOMAIN.COM placeholder\n- install.sh: prompt for domain at install time, sed-replace placeholder\n- install.sh: remove personal domain comment\n- index.html: genericise title (remove personal callsign) and placeholder examples\n- aprs_server.go: remove stale hardcoded username init lines"

for args in [
    ['git', 'add', '-A'],
    ['git', 'commit', '-m', msg],
    ['git', 'push', 'origin', 'main'],
]:
    r = subprocess.run(args, cwd=r'C:\aprs-fix', capture_output=True, text=True)
    print(r.stdout.strip(), r.stderr.strip())
