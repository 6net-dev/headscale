defmodule Registrar.Invites.Invite do
  use Ecto.Schema

  import Ecto.Changeset

  schema "invites" do
    belongs_to :org, Registrar.Orgs.Org

    field :token, :string
    field :identity, :string
    field :scope, :map, default: %{}
    field :expires_at, :utc_datetime_usec
    field :max_uses, :integer
    field :uses, :integer, default: 0

    timestamps(type: :utc_datetime_usec)
  end

  def changeset(invite, attrs) do
    invite
    |> cast(attrs, [:org_id, :token, :identity, :scope, :expires_at, :max_uses])
    |> validate_required([:org_id, :token, :identity])
    |> unique_constraint(:token)
  end
end
