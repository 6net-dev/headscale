defmodule Registrar.Orgs.SlugTest do
  use ExUnit.Case, async: true

  alias Registrar.Orgs.Slug

  test "accepts RFC 1123 labels" do
    for slug <- ~w(acme frites my-org a1 0day x2 big-corp-42) do
      assert Slug.valid?(slug), "expected #{slug} to be valid"
    end
  end

  test "rejects bad syntax" do
    refute Slug.valid?("a")
    refute Slug.valid?("-acme")
    refute Slug.valid?("acme-")
    refute Slug.valid?("Acme")
    refute Slug.valid?("ac.me")
    refute Slug.valid?("acmé")
    refute Slug.valid?(String.duplicate("a", 64))
    refute Slug.valid?("")
    refute Slug.valid?(nil)
  end

  test "rejects reserved words" do
    for slug <- ~w(www api admin derp dns registry internal) do
      refute Slug.valid?(slug), "expected #{slug} to be reserved"
    end
  end
end
