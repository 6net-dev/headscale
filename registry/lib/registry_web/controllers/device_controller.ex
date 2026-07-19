defmodule RegistrarWeb.DeviceController do
  use RegistrarWeb, :controller

  alias Registrar.Devices

  def create(conn, %{"name" => name, "node_key" => node_key}) do
    case Devices.enroll(%{name: name, node_key: node_key}) do
      {:ok, device, :created} ->
        conn
        |> put_status(:created)
        |> json(device_json(device))

      {:ok, device, :existing} ->
        json(conn, device_json(device))

      {:error, changeset} ->
        conn
        |> put_status(:unprocessable_entity)
        |> json(%{errors: errors_on_changeset(changeset)})
    end
  end

  def create(conn, _params) do
    conn
    |> put_status(:unprocessable_entity)
    |> json(%{errors: %{request: ["name and node_key are required"]}})
  end

  defp device_json(device) do
    %{id: device.id, name: device.name, node_key: device.node_key}
  end

  defp errors_on_changeset(changeset) do
    Ecto.Changeset.traverse_errors(changeset, fn {msg, opts} ->
      Regex.replace(~r"%{(\w+)}", msg, fn _, key ->
        opts |> Keyword.get(String.to_existing_atom(key), key) |> to_string()
      end)
    end)
  end
end
