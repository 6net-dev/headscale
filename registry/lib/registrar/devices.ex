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

  @doc """
  Wire-head enrollment: upsert a device by node key. New devices are
  auto-admitted (active) to the configured default org when one
  exists — the v0 bootstrap convenience standing in for the real
  admission flow. Existing devices are returned untouched.
  """
  def enroll(%{node_key: node_key} = attrs) do
    case get_device_by_node_key(node_key) do
      nil ->
        with {:ok, device} <- register_device(attrs) do
          maybe_default_admit(device)
          {:ok, device, :created}
        end

      device ->
        {:ok, device, :existing}
    end
  end

  defp maybe_default_admit(device) do
    with slug when is_binary(slug) <- Application.get_env(:registry, :default_org_slug),
         %{} = org <- Registrar.Orgs.get_org_by_slug(slug) do
      Registrar.Grants.admit(device, org, "enroll:#{device.node_key}", active: true)
    else
      _ -> :ok
    end
  end

  def get_device!(id), do: Repo.get!(Device, id)

  def get_device_by_node_key(node_key), do: Repo.get_by(Device, node_key: node_key)
end
