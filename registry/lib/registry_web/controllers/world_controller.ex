defmodule RegistrarWeb.WorldController do
  use RegistrarWeb, :controller

  alias Registrar.Devices
  alias Registrar.World

  def show(conn, %{"node_key" => node_key}) do
    case Devices.get_device_by_node_key(node_key) do
      nil ->
        conn
        |> put_status(:not_found)
        |> json(%{error: "unknown device"})

      device ->
        json(conn, World.for_device(device))
    end
  end
end
