defmodule RegistrarWeb.InviteController do
  use RegistrarWeb, :controller

  alias Registrar.{Invites, Net, Orgs}

  def create(conn, %{"slug" => slug, "identity" => identity} = params) do
    case Orgs.get_org_by_slug(slug) do
      nil ->
        conn
        |> put_status(:not_found)
        |> json(%{error: "unknown_org"})

      org ->
        {:ok, invite} =
          Invites.create(org,
            identity: identity,
            max_uses: params["max_uses"],
            expires_at: expires_at(params["expires_in_hours"])
          )

        conn
        |> put_status(:created)
        |> json(%{
          token: invite.token,
          org: slug,
          identity: invite.identity,
          max_uses: invite.max_uses,
          expires_at: invite.expires_at
        })
    end
  end

  def redeem(conn, %{"token" => token, "node_key" => node_key}) do
    case Invites.redeem(token, node_key) do
      {:ok, grant} ->
        {:ok, address} = Net.device_address(grant.org.prefix_index, grant.host_index)

        conn
        |> put_status(:created)
        |> json(%{org: grant.org.slug, active: grant.active, address: address})

      {:error, reason} when reason in [:invite_expired, :invite_exhausted] ->
        conn
        |> put_status(:gone)
        |> json(%{error: to_string(reason)})

      {:error, :already_admitted} ->
        conn
        |> put_status(:conflict)
        |> json(%{error: "already_admitted"})

      {:error, reason} ->
        conn
        |> put_status(:not_found)
        |> json(%{error: to_string(reason)})
    end
  end

  defp expires_at(nil), do: nil

  defp expires_at(hours) when is_integer(hours),
    do: DateTime.add(DateTime.utc_now(), hours * 3600, :second)
end
