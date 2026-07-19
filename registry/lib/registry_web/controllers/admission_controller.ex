defmodule RegistrarWeb.AdmissionController do
  use RegistrarWeb, :controller

  alias Registrar.{Grants, Net}

  def create(conn, %{"node_key" => node_key, "org" => org_slug} = params) do
    identity = Map.get(params, "identity", "admin")
    active = Map.get(params, "active", false)

    case Grants.admit_by(node_key, org_slug, identity, active: active) do
      {:ok, grant} ->
        {:ok, address} = Net.device_address(grant.org.prefix_index, grant.host_index)

        conn
        |> put_status(:created)
        |> json(%{
          org: org_slug,
          identity: grant.identity,
          active: grant.active,
          address: address
        })

      {:error, :already_admitted} ->
        conn
        |> put_status(:conflict)
        |> json(%{error: "already_admitted"})

      {:error, reason} when reason in [:unknown_device, :unknown_org] ->
        conn
        |> put_status(:not_found)
        |> json(%{error: to_string(reason)})

      {:error, _changeset} ->
        conn
        |> put_status(:unprocessable_entity)
        |> json(%{error: "invalid admission"})
    end
  end
end
