defmodule Registrar.Devices do
  @moduledoc """
  Device registry. A device registers once (one node key, one control
  connection); everything else it has, it has through grants.
  """

  alias Registrar.Devices.Device
  alias Registrar.Repo

  def register_device(attrs) do
    %Device{}
    |> Device.changeset(attrs)
    |> Repo.insert()
  end

  def get_device!(id), do: Repo.get!(Device, id)

  def get_device_by_node_key(node_key), do: Repo.get_by(Device, node_key: node_key)
end
