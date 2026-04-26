import subprocess

# ── install.sh - remove personal domain comment, keep repo URL ─────────────
install = open(r'C:\aprs-fix\install.sh', encoding='utf-8').read()
install = install.replace(
    '# Target: https://theloxleys.uk\n',
    ''
)
# REPO_URL is fine - it's the public GitHub repo
open(r'C:\aprs-fix\install.sh', 'w', encoding='utf-8').write(install)
print("install.sh saved")

# ── Caddyfile - replace personal domain with placeholder ───────────────────
caddy = open(r'C:\aprs-fix\Caddyfile', encoding='utf-8').read()
print("Caddyfile before:")
print(caddy)

new_caddy = """# Replace YOUR-DOMAIN.COM with your actual domain name
# This file is copied to /etc/caddy/Caddyfile by install.sh
YOUR-DOMAIN.COM {
    reverse_proxy /ws 127.0.0.1:8080
    reverse_proxy /api/* 127.0.0.1:8080
    reverse_proxy /setup 127.0.0.1:8080
    reverse_proxy /* 127.0.0.1:8080
}
"""
open(r'C:\aprs-fix\Caddyfile', 'w', encoding='utf-8').write(new_caddy)
print("\nCaddyfile after:")
print(new_caddy)

# ── README - update Caddyfile instructions to show placeholder ──────────────
readme = open(r'C:\aprs-fix\README.md', encoding='utf-8').read()
# Already no theloxleys in readme - confirm
print("readme theloxleys:", 'theloxleys' in readme.lower())
print("readme 33wf31:", '33wf31' in readme)
