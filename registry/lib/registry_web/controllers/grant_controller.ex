defmodule RegistrarWeb.GrantController do
  use RegistrarWeb, :controller

  alias Registrar.{Grants, Net}

  def index(conn, %{"node_key" => node_key}) do
    case Grants.list_for(node_key) do
      {:ok, device, grants} ->
        json(conn, %{
          device: %{name: device.name, node_key: device.node_key},
          grants: Enum.map(grants, &grant_json/1)
        })

      {:error, :unknown_device} ->
        conn
        |> put_status(:not_found)
        |> json(%{error: "unknown_device"})
    end
  end

  defp grant_json(grant) do
    {:ok, address} = Net.device_address(grant.org.prefix_index, grant.host_index)

    %{
      org: grant.org.slug,
      org_name: grant.org.name,
      identity: grant.identity,
      active: grant.active,
      address: address,
      admission_expires_at: grant.admission_expires_at
    }
  end

  def update(conn, %{"node_key" => node_key, "org" => org_slug, "active" => active})
      when is_boolean(active) do
    case Grants.set_active_by(node_key, org_slug, active) do
      {:ok, grant} ->
        json(conn, %{org: org_slug, active: grant.active})

      {:error, reason} ->
        conn
        |> put_status(:not_found)
        |> json(%{error: to_string(reason)})
    end
  end

  def update(conn, _params) do
    conn
    |> put_status(:unprocessable_entity)
    |> json(%{errors: %{request: ["boolean 'active' is required"]}})
  end
end
