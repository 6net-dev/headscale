defmodule RegistrarWeb.DeviceControllerTest do
  use RegistrarWeb.ConnCase, async: true

  alias Registrar.{Devices, Grants, Orgs}

  setup do
    {:ok, personal} = Orgs.create_org(%{slug: "personal", name: "Personal"})
    %{personal: personal}
  end

  test "POST /api/devices enrolls a new device with a default-org grant", %{conn: conn} do
    conn = post(conn, ~p"/api/devices", %{name: "mac", node_key: "nk:mac"})

    assert %{"name" => "mac", "node_key" => "nk:mac"} = json_response(conn, 201)

    device = Devices.get_device_by_node_key("nk:mac")
    assert [grant] = Grants.effective_grants()
    assert grant.device_id == device.id
    assert grant.org.slug == "personal"
  end

  test "enrollment is idempotent by node key", %{conn: conn} do
    post(conn, ~p"/api/devices", %{name: "mac", node_key: "nk:mac"})
    conn = post(conn, ~p"/api/devices", %{name: "mac-renamed", node_key: "nk:mac"})

    assert %{"name" => "mac"} = json_response(conn, 200)
    assert length(Grants.effective_grants()) == 1
  end

  test "enrollment without the default org still registers the device", %{
    conn: conn,
    personal: personal
  } do
    Registrar.Repo.delete!(personal)

    conn = post(conn, ~p"/api/devices", %{name: "mac", node_key: "nk:mac"})

    assert json_response(conn, 201)
    assert Devices.get_device_by_node_key("nk:mac")
    assert Grants.effective_grants() == []
  end

  test "invalid payloads are 422", %{conn: conn} do
    conn = post(conn, ~p"/api/devices", %{name: "mac"})
    assert json_response(conn, 422)["errors"]
  end
end
