# Security policy

## Supported versions

Security fixes go into the latest release only. Before reporting, check that the problem
still occurs on the [latest release](https://github.com/macourteau/asiair-commander/releases/latest).

## Reporting a vulnerability

Report it privately through GitHub's private vulnerability reporting: open the repository's
**Security** tab and choose **Report a vulnerability**. Do not open a public issue, pull request
or discussion for a vulnerability.

Include what you found, the version you tested, how you installed it (Docker or the Windows
installer), and the steps that reproduce it. A proof of concept helps, as long as it targets
only your own installation.

## Keep personal data out of the report

Leave out anything that identifies you, your network or your equipment unless the problem cannot
be shown without it: IP addresses, hostnames and domain names, file paths that contain user
names, ASIAIR device GUIDs, serials and Wi-Fi names, remote pairing values, passwords and
tokens, and FITS headers with site or observer data. Replace each with a placeholder such as
`192.0.2.10`, `nas.example` or `<guid>`. If a detail turns out to matter, you will be asked for it
privately.

## What to expect

You will get an acknowledgement of the report, an assessment of whether it is accepted, and a
fix in a release if it is. The fix is credited to you in the advisory unless you ask otherwise.
