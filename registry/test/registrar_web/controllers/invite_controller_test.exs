defmodule RegistrarWeb.InviteControllerTest do
  use RegistrarWeb.ConnCase, async: true

  alias Registrar.{Devices, Orgs}

  setup do
    {:ok, _} = Orgs.create_org(%{slug: "frites", name: "Frites"})
    {:ok, _} = Devices.register_device(%{name: "mac", node_key: "nk:mac"})
    :ok
  end

  test "create then redeem end-to-end", %{conn: conn} do
    conn1 = post(conn, ~p"/api/orgs/frites/invites", %{identity: "guest", max_uses: 2})
    assert %{"token" => token, "org" => "frites"} = json_response(conn1, 201)

    conn2 = post(conn, ~p"/api/invites/#{token}/redeem", %{node_key: "nk:mac"})
    assert %{"org" => "frites", "active" => true, "address" => addr} = json_response(conn2, 201)
    assert String.starts_with?(addr, "fd7a:115c:a1e0:")
  end

  test "redeeming an unknown token is 404", %{conn: conn} do
    conn = post(conn, ~p"/api/invites/bogus/redeem", %{node_key: "nk:mac"})
    assert json_response(conn, 404)["error"] == "unknown_invite"
  end

  test "exhausted invite is 410", %{conn: conn} do
    conn1 = post(conn, ~p"/api/orgs/frites/invites", %{identity: "g", max_uses: 1})
    token = json_response(conn1, 201)["token"]

    post(conn, ~p"/api/invites/#{token}/redeem", %{node_key: "nk:mac"})
    {:ok, _} = Devices.register_device(%{name: "pc", node_key: "nk:pc"})
    conn2 = post(conn, ~p"/api/invites/#{token}/redeem", %{node_key: "nk:pc"})

    assert json_response(conn2, 410)["error"] == "invite_exhausted"
  end

  test "invite for unknown org is 404", %{conn: conn} do
    conn = post(conn, ~p"/api/orgs/ghost/invites", %{identity: "g"})
    assert json_response(conn, 404)
  end
end
