defmodule Registrar.Orgs do
  @moduledoc """
  Org lifecycle. An org owns an immutable id, a unique slug, and the
  /56 selected by its prefix_index. The slug is a mutable pointer to
  the id — nothing but DNS and presentation may reference it.
  """

  import Ecto.Query

  alias Registrar.Orgs.Org
  alias Registrar.Repo

  def create_org(attrs) do
    Repo.transaction(fn ->
      next =
        (Repo.one(from o in Org, select: max(o.prefix_index)) || -1) + 1

      %Org{}
      |> Org.changeset(Map.put(attrs, :prefix_index, next))
      |> Repo.insert()
      |> case do
        {:ok, org} -> org
        {:error, changeset} -> Repo.rollback(changeset)
      end
    end)
  end

  def get_org!(id), do: Repo.get!(Org, id)

  def get_org_by_slug(slug), do: Repo.get_by(Org, slug: slug)

  def list_orgs, do: Repo.all(from o in Org, order_by: o.prefix_index)
end
