TorBridge Setup for Linux
=========================

Tor client for Linux (x86_64, systemd) that keeps itself connected through
working bridges. Everything needed is in this folder; the installation does
not download anything.

Install:    sudo ./install.sh
Uninstall:  sudo ./install.sh --uninstall

After installation:
  SOCKS5 proxy   127.0.0.1:9050 (9150 or 9250 if that port was already taken)
  HTTP proxy     127.0.0.1:9080 (9180 or 9280)
  Exit nodes     restricted to a list of countries (ExitNodes in /opt/torbridge/torrc)
  The installer prints the ports it chose.

How bridges are refreshed:
  - torbridge downloads fresh tested bridge lists from GitHub, shuffles them,
    probes them in parallel and tests each candidate with a real tor until
    10 working bridges are found, then restarts the tor service and verifies
    the connection (the previous bridges are restored if it fails).
  - torbridge-auto.timer runs daily at 10:00 and 10 minutes after boot: it
    refreshes bridges if they are older than 7 days or if the connection is down.
  - Every tested bridge is recorded in logs/history.jsonl. Bridge groups with a
    higher success rate are tried earlier (weighted random order), and a disabled
    transport is used again automatically once the history shows it works.

Useful commands:
  sudo torbridge status                          state of the service and connection
  sudo torbridge update                          find new bridges now (shows progress)
  sudo systemctl start torbridge-update.service  the same in the background
  sudo torbridge stats                           test every bridge type, write working
                                                 ones to /opt/torbridge/bridges-found.conf
  sudo torbridge stats -apply                    the same and apply them
  torbridge report                               success statistics from the history

Files: /opt/torbridge (tor, torrc, bridges.conf, logs/)
Units: torbridge-tor.service, torbridge-auto.timer, torbridge-update.service
