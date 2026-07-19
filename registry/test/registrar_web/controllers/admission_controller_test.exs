defmodule RegistrarWeb.AdmissionControllerTest do
  use RegistrarWeb.ConnCase, async: true

  alias Registrar.{Devices, Orgs, World}

  setup do
    {:ok, _personal} = Orgs.create_org(%{slug: "personal", name: "Personal"})
    {:ok, work} = Orgs.create_org(%{slug: "work", name: "Work"})
    {:ok, mac} = Devices.register_device(%{name: "mac", node_key: "nk:mac"})
    %{work: work, mac: mac}
  end

  test "POST admits a device to an org", %{conn: conn, mac: mac} do
    conn =
      post(conn, ~p"/api/devices/#{"nk:mac"}/grants", %{
        org: "work",
        identity: "james@work",
        active: true
      })

    assert %{"org" => "work", "active" => true, "address" => addr} = json_response(conn, 201)
    assert String.starts_with?(addr, "fd7a:115c:a1e0:1")
    assert World.for_device(mac).self.orgs == ["work"]
  end

  test "admission defaults to inactive", %{conn: conn, mac: mac} do
    conn = post(conn, ~p"/api/devices/#{"nk:mac"}/grants", %{org: "work", identity: "j"})

    assert json_response(conn, 201)["active"] == false
    assert World.for_device(mac).self.orgs == []
  end

  test "duplicate admission is 409", %{conn: conn} do
    post(conn, ~p"/api/devices/#{"nk:mac"}/grants", %{org: "work", identity: "j"})
    conn = post(conn, ~p"/api/devices/#{"nk:mac"}/grants", %{org: "work", identity: "j"})

    assert json_response(conn, 409)
  end

  test "unknown device or org is 404", %{conn: conn} do
    assert post(conn, ~p"/api/devices/#{"nk:ghost"}/grants", %{org: "work", identity: "j"})
           |> json_response(404)

    assert post(conn, ~p"/api/devices/#{"nk:mac"}/grants", %{org: "ghost", identity: "j"})
           |> json_response(404)
  end
end
