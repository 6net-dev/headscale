defmodule Registrar.Grants.Grant do
  use Ecto.Schema

  import Ecto.Changeset

  schema "grants" do
    belongs_to :device, Registrar.Devices.Device
    belongs_to :org, Registrar.Orgs.Org

    field :identity, :string
    field :scope, :map, default: %{}
    field :host_index, :integer
    field :active, :boolean, default: false
    field :admission_expires_at, :utc_datetime_usec

    timestamps(type: :utc_datetime_usec)
  end

  def changeset(grant, attrs) do
    grant
    |> cast(attrs, [
      :device_id,
      :org_id,
      :identity,
      :scope,
      :host_index,
      :active,
      :admission_expires_at
    ])
    |> validate_required([:device_id, :org_id, :identity, :host_index])
    |> validate_number(:host_index, greater_than: 0, less_than_or_equal_to: 0xFFFF)
    |> unique_constraint([:device_id, :org_id])
    |> unique_constraint([:org_id, :host_index])
  end

  def effective?(%__MODULE__{active: false}, _now), do: false

  def effective?(%__MODULE__{admission_expires_at: nil}, _now), do: true

  def effective?(%__MODULE__{admission_expires_at: exp}, now),
    do: DateTime.after?(exp, now)
end
