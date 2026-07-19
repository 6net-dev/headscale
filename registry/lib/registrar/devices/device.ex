defmodule Registrar.Devices.Device do
  use Ecto.Schema

  import Ecto.Changeset

  schema "devices" do
    field :name, :string
    field :node_key, :string

    has_many :grants, Registrar.Grants.Grant

    timestamps(type: :utc_datetime_usec)
  end

  def changeset(device, attrs) do
    device
    |> cast(attrs, [:name, :node_key])
    |> validate_required([:name, :node_key])
    |> unique_constraint(:node_key)
  end
end
