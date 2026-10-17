#!/usr/bin/env bash
# Authenticode-sign a Windows PE in place, via Azure Artifact Signing.
#
# Called three times per signed installer build: once from the Makefile for the
# packaged binary, then twice by makensis itself (!uninstfinalize, !finalize)
# for the uninstaller stub and the finished setup executable. Ordering is
# load-bearing — the payload has to be signed before makensis compresses it,
# because a file inside the installer's LZMA blob is no longer a PE and can
# never be signed afterwards.
#
# jsign rather than Microsoft's signtool: the whole release pipeline is Linux,
# and signtool, `dotnet sign` and azure/artifact-signing-action are all
# Windows-only by design. jsign speaks the same signing REST API, so the
# installer keeps building on one runner.
#
# Credentials come from the ambient `az` session that azure/login establishes
# from the job's OIDC token. Nothing is stored, and there is no client secret.
set -euo pipefail

target=${1:?usage: sign-windows.sh <file>}

: "${AC_SIGN_ENDPOINT:?required, e.g. <region>.codesigning.azure.net}"
: "${AC_SIGN_ALIAS:?required, e.g. <signing-account>/<certificate-profile>}"
: "${JSIGN_JAR:?required, path to jsign-<version>.jar}"

# Artifact Signing certificates are valid for 72 hours, so an untimestamped
# signature stops verifying three days after release with no way to repair it
# short of re-signing and re-publishing. Timestamping is not optional here.
readonly TSA_URL="http://timestamp.acs.microsoft.com"

echo "sign-windows: signing ${target} as ${AC_SIGN_ALIAS} via ${AC_SIGN_ENDPOINT}"

# Requested without a trailing slash: the CLI will happily mint a token for
# ".../codesigning.azure.net/", which the service then rejects with a 401 that
# reads like an authorization problem rather than a URL typo.
AZ_TOKEN=$(az account get-access-token \
	--resource https://codesigning.azure.net \
	--query accessToken -o tsv)
export AZ_TOKEN

# --storepass reads the variable by name so the token never lands in ps(1)
# output, a shell history, or a CI log line.
java -jar "${JSIGN_JAR}" \
	--storetype TRUSTEDSIGNING \
	--keystore "${AC_SIGN_ENDPOINT}" \
	--storepass env:AZ_TOKEN \
	--alias "${AC_SIGN_ALIAS}" \
	--alg SHA-256 \
	--tsaurl "${TSA_URL}" \
	--tsmode RFC3161 \
	"${target}"

echo "sign-windows: signed ${target}"
