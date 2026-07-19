defmodule RegistrarWeb.GrantListTest do
  use RegistrarWeb.ConnCase, async: true

  alias Registrar.{Devices, Grants, Orgs}

  setup do
    {:ok, personal} = Orgs.create_org(%{slug: "personal", name: "Personal"})
    {:ok, work} = Orgs.create_org(%{slug: "work", name: "Work"})
    {:ok, mac} = Devices.register_device(%{name: "mac", node_key: "nk:mac"})
    {:ok, _} = Grants.admit(mac, personal, "james", active: true)
    {:ok, _} = Grants.admit(mac, work, "james@work", active: false)
    :ok
  end

  test "GET lists all grants with held addresses, active or not", %{conn: conn} do
    conn = get(conn, ~p"/api/devices/#{"nk:mac"}/grants")

    assert %{"device" => %{"name" => "mac"}, "grants" => grants} = json_response(conn, 200)
    assert [%{"org" => "personal"} = personal, %{"org" => "work"} = work] =
             Enum.sort_by(grants, & &1["org"])

    assert personal["active"] == true
    assert String.starts_with?(personal["address"], "fd7a:115c:a1e0:")
    assert work["active"] == false
    assert work["address"] != nil, "inactive grants still hold their address slot"
    assert work["identity"] == "james@work"
  end

  test "unknown device is 404", %{conn: conn} do
    assert get(conn, ~p"/api/devices/#{"nk:ghost"}/grants") |> json_response(404)
  end
end
