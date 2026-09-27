TorBridge Setup
===============

Tor client for Windows 10/11 that keeps itself connected through working bridges
(webtunnel, plain/vanilla). Everything needed is in this folder;
the installation does not download Tor from the internet.

Install:    double-click Install.cmd (asks for administrator rights)
Uninstall:  double-click Uninstall.cmd

After installation:
  SOCKS5 proxy   127.0.0.1:9050
  HTTP proxy     127.0.0.1:9080
  Exit nodes     restricted to a list of countries (ExitNodes in C:\Tor\torrc)

How bridges are refreshed:
  - torbridge downloads fresh tested bridge lists from GitHub, shuffles them,
    probes them in parallel and tests each candidate with a real tor until
    10 working bridges are found, then reloads the tor service and verifies
    the connection (the previous bridges are restored if it fails).
  - Scheduled task \TorBridge\Auto runs daily at 10:00 and 10 minutes after
    boot: it refreshes bridges if they are older than 7 days or if the
    connection is down. It only runs when the network is available.

  - Every tested bridge is recorded in logs/history.jsonl. Bridge groups with a
    higher success rate are tried earlier (weighted random order), and a disabled
    transport is used again automatically once the history shows it works.

Useful commands (administrator command prompt):
  torbridge status                        state of the service and connection
  torbridge update                        find new bridges now (shows progress)
  schtasks /run /tn \TorBridge\UpdateNow  the same in the background
  torbridge stats                         test every bridge type, write working
                                          ones to C:\Tor\bridges-found.conf
  torbridge stats -apply                  the same and apply them
  torbridge report                        success statistics from the history

Logs: C:\Tor\logs\torbridge.log, C:\Tor\logs\tor.log
