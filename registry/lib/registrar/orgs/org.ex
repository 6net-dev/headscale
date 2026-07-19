defmodule Registrar.Orgs.Org do
  use Ecto.Schema

  import Ecto.Changeset

  alias Registrar.Orgs.Slug

  schema "orgs" do
    field :slug, :string
    field :name, :string
    field :prefix_index, :integer

    has_many :grants, Registrar.Grants.Grant

    timestamps(type: :utc_datetime_usec)
  end

  def changeset(org, attrs) do
    org
    |> cast(attrs, [:slug, :name, :prefix_index])
    |> validate_required([:slug, :name, :prefix_index])
    |> validate_change(:slug, fn :slug, slug ->
      if Slug.valid?(slug), do: [], else: [slug: "must be a valid, non-reserved DNS label"]
    end)
    |> validate_number(:prefix_index,
      greater_than_or_equal_to: 0,
      less_than_or_equal_to: 255
    )
    |> unique_constraint(:slug)
    |> unique_constraint(:prefix_index)
  end
end
