defmodule RegistrarWeb.GrantController do
  use RegistrarWeb, :controller

  alias Registrar.Grants

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
