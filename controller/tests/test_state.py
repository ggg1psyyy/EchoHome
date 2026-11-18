"""em_state's setters: what main() records for the routes that read it."""

import em_state


def test_the_tls_directory_is_recorded(monkeypatch):
    monkeypatch.setattr(em_state, "tls_dir", None)
    em_state.set_tls_dir("/data/tls")
    assert em_state.tls_dir == "/data/tls"


def test_the_link_ports_are_recorded_and_a_missing_address_is_empty(monkeypatch):
    monkeypatch.setattr(em_state, "link", {"ip": "", "port": 8767, "tls_port": 0})
    em_state.set_link_ports("10.0.0.2", 8767, 8770)
    assert em_state.link == {"ip": "10.0.0.2", "port": 8767, "tls_port": 8770}
    em_state.set_link_ports(None, 8767, 0)
    assert em_state.link == {"ip": "", "port": 8767, "tls_port": 0}
