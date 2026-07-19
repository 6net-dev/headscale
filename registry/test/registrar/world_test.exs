defmodule Registrar.WorldTest do
  use Registrar.DataCase, async: true

  alias Registrar.{Devices, Grants, Orgs, World}

  # Cast: mac holds grants in personal AND acme; core7 is personal-only;
  # workstation is acme-only. This is the smallest world where the org
  # seam matters: core7 must never learn mac's acme address.
  setup do
    {:ok, personal} = Orgs.create_org(%{slug: "personal", name: "Personal"})
    {:ok, acme} = Orgs.create_org(%{slug: "acme", name: "Acme Corp"})

    {:ok, mac} = Devices.register_device(%{name: "mac", node_key: "nk:mac"})
    {:ok, core7} = Devices.register_device(%{name: "core7", node_key: "nk:core7"})
    {:ok, ws} = Devices.register_device(%{name: "workstation", node_key: "nk:ws"})

    {:ok, mac_personal} = Grants.admit(mac, personal, "james", active: true)
    {:ok, mac_acme} = Grants.admit(mac, acme, "james@acme", active: true)
    {:ok, _} = Grants.admit(core7, personal, "james", active: true)
    {:ok, _} = Grants.admit(ws, acme, "james@acme", active: true)

    %{
      personal: personal,
      acme: acme,
      mac: mac,
      core7: core7,
      ws: ws,
      mac_personal: mac_personal,
      mac_acme: mac_acme
    }
  end

  test "self addresses: one per active grant, in the org's /56", %{mac: mac} do
    world = World.for_device(mac)

    assert length(world.self.addresses) == 2
    assert Enum.any?(world.self.addresses, &String.starts_with?(&1, "fd7a:115c:a1e0:"))
    assert Enum.sort(world.self.orgs) == ["acme", "personal"]
  end

  test "peers appear only through shared orgs", %{mac: mac} do
    world = World.for_device(mac)

    assert Enum.map(world.peers, & &1.name) |> Enum.sort() == ["core7", "workstation"]
  end

  test "the org seam: peers expose only shared-org addresses", %{
    core7: core7,
    acme: acme
  } do
    world = World.for_device(core7)

    assert [mac_peer] = world.peers
    assert mac_peer.name == "mac"
    assert length(mac_peer.addresses) == 1

    {:ok, acme_prefix} = Registrar.Net.org_prefix(acme.prefix_index)
    acme_net = String.trim_trailing(acme_prefix, "::/56")

    refute Enum.any?(mac_peer.addresses, &String.starts_with?(&1, acme_net)),
           "core7 must not learn mac's acme address"
  end

  test "deactivation removes the org's world additively", %{
    mac: mac,
    mac_acme: mac_acme
  } do
    {:ok, _} = Grants.deactivate(mac_acme)
    world = World.for_device(mac)

    assert world.self.orgs == ["personal"]
    assert length(world.self.addresses) == 1
    assert Enum.map(world.peers, & &1.name) == ["core7"]
  end

  test "expired admissions do not compose", %{mac: mac, ws: ws, acme: acme} do
    past = DateTime.add(DateTime.utc_now(), -3600, :second)
    {:ok, _} = Grants.revoke_all(ws, acme)
    {:ok, _} = Grants.admit(ws, acme, "james@acme", active: true, expires_at: past)

    world = World.for_device(mac)
    refute "workstation" in Enum.map(world.peers, & &1.name)
  end

  test "search domains follow active grants", %{mac: mac} do
    world = World.for_device(mac)

    assert Enum.sort(world.dns.search_domains) ==
             ["acme.overlay.internal", "personal.overlay.internal"]
  end

  test "v1 entries carry org-scoped fqdns", %{mac: mac} do
    world = World.for_device(mac)

    assert world.version == 1
    assert world.self.name == "mac"

    fqdns = Enum.map(world.self.entries, & &1.fqdn) |> Enum.sort()
    assert fqdns == ["mac.acme.overlay.internal", "mac.personal.overlay.internal"]

    assert world.dns.base_domain == "overlay.internal"

    [core7_peer] = Enum.filter(world.peers, &(&1.name == "core7"))
    assert [%{org: "personal", fqdn: "core7.personal.overlay.internal"}] =
             Enum.map(core7_peer.entries, &Map.take(&1, [:org, :fqdn]))
  end
end
