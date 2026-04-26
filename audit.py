src    = open(r'C:\aprs-fix\aprs_server.go', encoding='utf-8').read()
html   = open(r'C:\aprs-fix\index.html', encoding='utf-8').read()
readme = open(r'C:\aprs-fix\README.md', encoding='utf-8').read()
install= open(r'C:\aprs-fix\install.sh', encoding='utf-8').read()
caddy  = open(r'C:\aprs-fix\Caddyfile', encoding='utf-8').read()
gi     = open(r'C:\aprs-fix\.gitignore', encoding='utf-8').read()

checks = [
    ('go:      2e0lxy hardcoded',    '2e0lxy' in src.lower()),
    ('go:      33wf31ug33 password', '33wf31' in src),
    ('go:      theloxleys domain',   'theloxleys' in src),
    ('go:      53.7264 coords',      '53.7264' in src),
    ('go:      -1.5744 coords',      '-1.5744' in src),
    ('go:      NOCALL default',      '"NOCALL"' in src),
    ('html:    2e0lxy hardcoded',    '2e0lxy' in html.lower()),
    ('html:    33wf31ug33 password', '33wf31' in html),
    ('html:    theloxleys domain',   'theloxleys' in html),
    ('html:    53.7264 coords',      '53.7264' in html),
    ('readme:  theloxleys domain',   'theloxleys' in readme.lower()),
    ('readme:  33wf31 password',     '33wf31' in readme),
    ('install: theloxleys domain',   'theloxleys' in install),
    ('install: 2e0lxy',              '2e0lxy' in install.lower()),
    ('caddy:   theloxleys domain',   'theloxleys' in caddy),
    ('gitignore: creds.json',        'creds.json' in gi),
    ('gitignore: server_config',     'server_config.json' in gi),
]

for name, found in checks:
    print(('!! FOUND  ' if found else '   ok     ') + name)
