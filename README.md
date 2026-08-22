# mera-extractor
Uses `mxcli` alpha project by `Mendix Labs` and actual Mendix binaries to extract changes and analyze them

# Implementing new Mendix version
`./scrips/fetch-mx.sh` is created to extract and trim actual Mendix binaries that are use for `mx diff`

# known issues
- Works for diff on the same Mendix version. Only a small subset of Mendix version is supported (see mx-versions.txt)
- Diffing and analyzing does not work for version upgrade commits