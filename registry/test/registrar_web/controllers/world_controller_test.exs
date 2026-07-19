defmodule RegistrarWeb.WorldControllerTest do
  use RegistrarWeb.ConnCase, async: true

  alias Registrar.{Devices, Grants, Orgs}

  setup do
    {:ok, personal} = Orgs.create_org(%{slug: "personal", name: "Personal"})
    {:ok, mac} = Devices.register_device(%{name: "mac", node_key: "nk:mac"})
    {:ok, core7} = Devices.register_device(%{name: "core7", node_key: "nk:core7"})
    {:ok, _} = Grants.admit(mac, personal, "james", active: true)
    {:ok, _} = Grants.admit(core7, personal, "james", active: true)
    :ok
  end

  test "GET /api/world/:node_key returns the composed world", %{conn: conn} do
    conn = get(conn, ~p"/api/world/#{"nk:mac"}")

    assert %{
             "version" => 0,
             "self" => %{"addresses" => [addr], "orgs" => ["personal"]},
             "peers" => [%{"name" => "core7", "node_key" => "nk:core7"}],
             "dns" => %{"search_domains" => ["personal.overlay.internal"]}
           } = json_response(conn, 200)

    assert String.starts_with?(addr, "fd7a:115c:a1e0:")
  end

  test "unknown node key is 404", %{conn: conn} do
    conn = get(conn, ~p"/api/world/#{"nk:nope"}")
    assert json_response(conn, 404)["error"] == "unknown device"
  end
end
