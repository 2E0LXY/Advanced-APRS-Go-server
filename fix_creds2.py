import subprocess

# ── index.html ─────────────────────────────────────────────────────────────
html = open(r'C:\aprs-fix\index.html', encoding='utf-8').read()

# Title - remove personal callsign
html = html.replace('<title>APRS Gateway — 2E0LXY</title>', '<title>Advanced APRS Gateway</title>')

# Placeholder example callsign - keep as generic example
html = html.replace(
    'placeholder="Callsigns: 2E0LXY-9 M0XYZ G4ABC*"',
    'placeholder="e.g. M0ABC-9 G4XYZ* (wildcards ok)"'
)
html = html.replace(
    'placeholder="Your callsign (e.g. 2E0LXY)"',
    'placeholder="Your callsign (e.g. M0ABC)"'
)

open(r'C:\aprs-fix\index.html', 'w', encoding='utf-8').write(html)
print("html saved")

# ── install.sh ─────────────────────────────────────────────────────────────
install = open(r'C:\aprs-fix\install.sh', encoding='utf-8').read()
print("\ninstall.sh before:")
for i, l in enumerate(install.split('\n')):
    if 'theloxleys' in l.lower() or '2e0lxy' in l.lower():
        print(f"  line {i+1}: {l}")
