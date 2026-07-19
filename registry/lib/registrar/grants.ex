defmodule Registrar.Grants do
  @moduledoc """
  The grant is the atom: (device, org, identity, scope,
  admission-expiry, active). Admission creates a grant and allocates
  its host_index (and therefore its address) inside the org's /56;
  activation is an instant toggle; revocation deletes the grant and
  frees the slot.
  """

  import Ecto.Query

  alias Registrar.Grants.Grant
  alias Registrar.Repo

  @world_changes_topic "world_changes"

  def world_changes_topic, do: @world_changes_topic

  def set_active_by(node_key, org_slug, active) when is_boolean(active) do
    device = Registrar.Devices.get_device_by_node_key(node_key)
    org = Registrar.Orgs.get_org_by_slug(org_slug)

    cond do
      is_nil(device) ->
        {:error, :unknown_device}

      is_nil(org) ->
        {:error, :unknown_org}

      true ->
        case Repo.get_by(Grant, device_id: device.id, org_id: org.id) do
          nil -> {:error, :no_grant}
          grant -> set_active(grant, active)
        end
    end
  end

  def admit(device, org, identity, opts \\ []) do
    Repo.transaction(fn ->
      next =
        (Repo.one(from g in Grant, where: g.org_id == ^org.id, select: max(g.host_index)) || 0) +
          1

      %Grant{}
      |> Grant.changeset(%{
        device_id: device.id,
        org_id: org.id,
        identity: identity,
        scope: Keyword.get(opts, :scope, %{}),
        host_index: next,
        active: Keyword.get(opts, :active, false),
        admission_expires_at: Keyword.get(opts, :expires_at)
      })
      |> Repo.insert()
      |> case do
        {:ok, grant} -> grant
        {:error, changeset} -> Repo.rollback(changeset)
      end
    end)
    |> tap(fn
      {:ok, _grant} -> broadcast_world_change(org.id)
      _ -> :ok
    end)
  end

  def activate(%Grant{} = grant), do: set_active(grant, true)

  def deactivate(%Grant{} = grant), do: set_active(grant, false)

  def revoke_all(device, org) do
    # Compute the audience before deleting so the revoked device still
    # hears about its own removal.
    keys = org_node_keys(org.id)

    {count, _} =
      Repo.delete_all(from g in Grant, where: g.device_id == ^device.id and g.org_id == ^org.id)

    broadcast(keys)

    {:ok, count}
  end

  def effective_grants do
    now = DateTime.utc_now()

    Repo.all(from g in Grant, where: g.active, preload: [:device, :org])
    |> Enum.filter(&Grant.effective?(&1, now))
  end

  defp set_active(grant, active) do
    grant
    |> Ecto.Changeset.change(active: active)
    |> Repo.update()
    |> tap(fn
      {:ok, updated} -> broadcast_world_change(updated.org_id)
      _ -> :ok
    end)
  end

  # Conservative audience: every device holding a grant (any state) in
  # the changed org. Over-notifying is harmless — the wire head
  # re-fetches worlds; under-notifying strands stale netmaps.
  defp broadcast_world_change(org_id), do: org_id |> org_node_keys() |> broadcast()

  defp org_node_keys(org_id) do
    Repo.all(
      from g in Grant,
        join: d in assoc(g, :device),
        where: g.org_id == ^org_id,
        distinct: true,
        select: d.node_key
    )
  end

  defp broadcast([]), do: :ok

  defp broadcast(node_keys) do
    Phoenix.PubSub.broadcast(
      Registrar.PubSub,
      @world_changes_topic,
      {:world_changed, node_keys}
    )
  end
end
