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
  end

  def activate(%Grant{} = grant), do: set_active(grant, true)

  def deactivate(%Grant{} = grant), do: set_active(grant, false)

  def revoke_all(device, org) do
    {count, _} =
      Repo.delete_all(from g in Grant, where: g.device_id == ^device.id and g.org_id == ^org.id)

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
  end
end
