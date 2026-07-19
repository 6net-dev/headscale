defmodule RegistrarWeb.GrantControllerTest do
  use RegistrarWeb.ConnCase, async: true

  alias Registrar.{Devices, Grants, Orgs, World}

  setup do
    {:ok, personal} = Orgs.create_org(%{slug: "personal", name: "Personal"})
    {:ok, mac} = Devices.register_device(%{name: "mac", node_key: "nk:mac"})
    {:ok, grant} = Grants.admit(mac, personal, "james", active: true)
    %{mac: mac, grant: grant}
  end

  test "PUT toggles a grant and announces the world change", %{conn: conn, mac: mac} do
    :ok = Phoenix.PubSub.subscribe(Registrar.PubSub, "world_changes")

    conn = put(conn, ~p"/api/devices/#{"nk:mac"}/grants/personal", %{active: false})

    assert json_response(conn, 200) == %{"org" => "personal", "active" => false}
    assert_world_change_for("nk:mac")
    assert World.for_device(mac).self.addresses == []
  end

  test "reactivation restores the world", %{conn: conn, mac: mac} do
    put(conn, ~p"/api/devices/#{"nk:mac"}/grants/personal", %{active: false})
    conn = put(conn, ~p"/api/devices/#{"nk:mac"}/grants/personal", %{active: true})

    assert json_response(conn, 200)["active"] == true
    assert length(World.for_device(mac).self.addresses) == 1
  end

  test "unknown device, org, or grant is 404", %{conn: conn} do
    assert put(conn, ~p"/api/devices/#{"nk:ghost"}/grants/personal", %{active: false})
           |> json_response(404)

    assert put(conn, ~p"/api/devices/#{"nk:mac"}/grants/ghost-org", %{active: false})
           |> json_response(404)
  end

  test "non-boolean active is 422", %{conn: conn} do
    conn = put(conn, ~p"/api/devices/#{"nk:mac"}/grants/personal", %{active: "yes"})
    assert json_response(conn, 422)["errors"]
  end

  defp assert_world_change_for(key) do
    assert_receive {:world_changed, keys}, 500

    if key in keys, do: :ok, else: assert_world_change_for(key)
  end
end
