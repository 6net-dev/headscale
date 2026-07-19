defmodule Registrar.InvitesTest do
  use Registrar.DataCase, async: true

  alias Registrar.{Devices, Invites, Orgs, World}

  setup do
    {:ok, org} = Orgs.create_org(%{slug: "frites", name: "Frites"})
    {:ok, mac} = Devices.register_device(%{name: "mac", node_key: "nk:mac"})
    %{org: org, mac: mac}
  end

  test "create issues an opaque token bound to the org", %{org: org} do
    {:ok, invite} = Invites.create(org, identity: "guest", max_uses: 5)

    assert byte_size(invite.token) >= 16
    assert invite.org_id == org.id
    assert invite.uses == 0
  end

  test "redeem admits the device with an active grant", %{org: org, mac: mac} do
    {:ok, invite} = Invites.create(org, identity: "guest")
    {:ok, grant} = Invites.redeem(invite.token, "nk:mac")

    assert grant.active
    assert grant.identity == "guest"
    assert World.for_device(mac).self.orgs == ["frites"]
  end

  test "redeem enforces expiry and use limits", %{org: org, mac: mac} do
    past = DateTime.add(DateTime.utc_now(), -60, :second)
    {:ok, expired} = Invites.create(org, identity: "g", expires_at: past)
    assert {:error, :invite_expired} = Invites.redeem(expired.token, "nk:mac")

    {:ok, single} = Invites.create(org, identity: "g", max_uses: 1)
    {:ok, other} = Devices.register_device(%{name: "pc", node_key: "nk:pc"})
    {:ok, _} = Invites.redeem(single.token, other.node_key)
    assert {:error, :invite_exhausted} = Invites.redeem(single.token, "nk:mac")
  end

  test "redeem is rejected for unknown tokens and devices", %{org: org} do
    assert {:error, :unknown_invite} = Invites.redeem("nope", "nk:mac")

    {:ok, invite} = Invites.create(org, identity: "g")
    assert {:error, :unknown_device} = Invites.redeem(invite.token, "nk:ghost")
  end

  test "double redemption by the same device conflicts", %{org: org, mac: mac} do
    {:ok, invite} = Invites.create(org, identity: "g")
    {:ok, _} = Invites.redeem(invite.token, mac.node_key)
    assert {:error, :already_admitted} = Invites.redeem(invite.token, mac.node_key)
  end
end
