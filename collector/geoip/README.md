# collector/geoip/

Drop the MaxMind **GeoLite2-City** database here as `GeoLite2-City.mmdb`.

The collector reads it via `GEOIP_DB_PATH` (defaulted to `/geoip/GeoLite2-City.mmdb`
in the image by `../Dockerfile`, which `COPY`s this whole directory). Loading is
**best-effort**: if the file is missing the service still boots and every geo
field is left empty — geo is enrichment, not a gate on ingest.

## Getting the file

1. Free MaxMind account → https://www.maxmind.com/en/geolite2/signup
2. Download the **GeoLite2 City** database (`.mmdb`, ~60 MB).
3. Place it here as `GeoLite2-City.mmdb` and commit it.

## Notes

- **License:** the GeoLite2 EULA forbids *public* redistribution. Keep this repo
  private. Do not open-source it with the `.mmdb` in history.
- **Git bloat:** the file is a ~60 MB binary. If you plan to refresh it regularly
  (MaxMind updates twice weekly), track `*.mmdb` with Git LFS so history doesn't
  balloon. A one-off commit with rare updates is fine as a plain blob.
- The database gives country / region (subdivision) / city / lat / lon. ASN
  (ISP/network) would need a separate `GeoLite2-ASN.mmdb` and new proto fields —
  not wired today.
