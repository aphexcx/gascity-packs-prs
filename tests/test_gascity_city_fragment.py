"""The city fragment supplies only the parked-worker mail recipient."""

from pathlib import Path
import tomllib


REPO_ROOT = Path(__file__).resolve().parents[1]


def test_gascity_city_fragment_routes_park_alerts_to_mayor() -> None:
    fragment = REPO_ROOT / "gascity" / "city-fragment.toml"
    config = tomllib.loads(fragment.read_text(encoding="utf-8"))

    # Exact equality also excludes unrelated settings and an empty recipient.
    assert config == {"session": {"park_alert_to": "mayor"}}
