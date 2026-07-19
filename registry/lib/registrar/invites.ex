defmodule Registrar.Invites do
  @moduledoc """
  Invite links: the self-service face of admission. An invite is a
  capability token bound to an org — whoever presents it gets a grant
  with the invite's identity and scope, subject to expiry and use
  limits. Redemption creates an *active* grant: redeeming is a user
  act, so the subnet turns on immediately; the toggle takes it from
  there.
  """

  import Ecto.Query

  alias Registrar.Grants
  alias Registrar.Invites.Invite
  alias Registrar.Repo

  def create(org, opts) do
    %Invite{}
    |> Invite.changeset(%{
      org_id: org.id,
      token: generate_token(),
      identity: Keyword.fetch!(opts, :identity),
      scope: Keyword.get(opts, :scope, %{}),
      expires_at: Keyword.get(opts, :expires_at),
      max_uses: Keyword.get(opts, :max_uses)
    })
    |> Repo.insert()
  end

  def redeem(token, node_key) do
    now = DateTime.utc_now()
    invite = Repo.one(from i in Invite, where: i.token == ^token, preload: [:org])
    device = Registrar.Devices.get_device_by_node_key(node_key)

    cond do
      is_nil(invite) ->
        {:error, :unknown_invite}

      not is_nil(invite.expires_at) and DateTime.before?(invite.expires_at, now) ->
        {:error, :invite_expired}

      not is_nil(invite.max_uses) and invite.uses >= invite.max_uses ->
        {:error, :invite_exhausted}

      is_nil(device) ->
        {:error, :unknown_device}

      true ->
        with {:ok, grant} <-
               Grants.admit_by(node_key, invite.org.slug, invite.identity,
                 scope: invite.scope,
                 active: true
               ) do
          {1, _} =
            Repo.update_all(from(i in Invite, where: i.id == ^invite.id),
              inc: [uses: 1]
            )

          {:ok, grant}
        end
    end
  end

  defp generate_token do
    16 |> :crypto.strong_rand_bytes() |> Base.url_encode64(padding: false)
  end
end
